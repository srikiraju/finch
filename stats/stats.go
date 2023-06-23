// Copyright 2023 Block, Inc.

package stats

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
)

var nEventTypes = 4 // number of event types:

const (
	READ byte = iota
	WRITE
	COMMIT
	TOTAL
)

// Stats are lock-free basic statistics: query count (N), min and max response time,
// and response time distribution and percentiles using the same histogram bucketes
// as MySQL 8.0. All times are microseconds.
//
// Finch calculates stats per-client and per-trx (Finch trx file, not MySQL trx).
// If there are 8 clients running 2 trx, then there are 16 instances of Stats
// which is half of the lock-free design. The other half is Trx.
type Stats struct {
	Buckets [][]uint64        // response time (μs) for percentiles
	Min     []int64           // response time (μs)
	Max     []int64           // response time (μs)
	N       []uint64          // number of events (queries)
	Errors  map[uint16]uint64 // count MySQL error codes
}

func NewStats() *Stats {
	// Buckets for each event type
	buckets := make([][]uint64, nEventTypes)
	for i := range buckets {
		buckets[i] = make([]uint64, n_buckets)
	}
	return &Stats{
		Buckets: buckets,
		Min:     make([]int64, nEventTypes),
		Max:     make([]int64, nEventTypes),
		N:       make([]uint64, nEventTypes),
		Errors:  map[uint16]uint64{},
	}
}

// https://dev.mysql.com/worklog/task/?id=5384
const n_buckets = 450
const base = 10.0                   // microseconds
const factor = 1.0471285480508996   // 4.7% bucket size increments
const logFactor = 0.046051701859881 // ln(factor)

// Record records the duration of an event in microseconds.
func (s *Stats) Record(eventType byte, d int64) {
	// Calculate bucket number
	bucket := math.Log(float64(d)/base) / logFactor
	n := uint(bucket) + 1
	if bucket < 0 {
		n = 0
	}
	if n > n_buckets-1 {
		n = n_buckets - 1
	}

	// Record event types separately
	s.Buckets[eventType][n] += 1
	if d < s.Min[eventType] || s.N[eventType] == 0 {
		s.Min[eventType] = d
	}
	if d > s.Max[eventType] {
		s.Max[eventType] = d
	}
	s.N[eventType]++

	// Also record non-TOTAL events in the total stats. Since TOTAL events are
	// recoded above, only do this for non-TOTAL events.
	if eventType != TOTAL {
		s.Buckets[TOTAL][n] += 1
		if d < s.Min[TOTAL] || s.N[TOTAL] == 0 {
			s.Min[TOTAL] = d
		}
		if d > s.Max[TOTAL] {
			s.Max[TOTAL] = d
		}
		s.N[TOTAL]++
	}
}

// Reset resets all values to zero.
func (s *Stats) Reset() {
	for i := 0; i < nEventTypes; i++ {
		for j := range s.Buckets[i] {
			s.Buckets[i][j] = 0
		}
		s.Min[i] = 0
		s.Max[i] = 0
		s.N[i] = 0
	}
	for k := range s.Errors {
		s.Errors[k] = 0
	}
}

// Copy copies all stats from c, overwriting all values in s. Calling Reset before
// Copy is not necessary because the copy overwrites all values.
func (s *Stats) Copy(c *Stats) {
	for i := 0; i < nEventTypes; i++ {
		copy(s.Buckets[i], c.Buckets[i])
		s.Min[i] = c.Min[i]
		s.Max[i] = c.Max[i]
		s.N[i] = c.N[i]
	}
	for k, v := range c.Errors {
		s.Errors[k] = v
	}
}

// Combine combines all stats from c. All values in s are adjusted with respect
// to c. For example, of c.Min < s.Min, then s.Min = c.Min. s is modified, but c
// is not. This is used to create total stats in the Collector and reporters.
func (s *Stats) Combine(c *Stats) {
	for i := 0; i < nEventTypes; i++ {
		for j := range s.Buckets[i] {
			s.Buckets[i][j] += c.Buckets[i][j]
		}
		if c.Min[i] < s.Min[i] || s.N[i] == 0 {
			s.Min[i] = c.Min[i]
		}
		if c.Max[i] > s.Max[i] {
			s.Max[i] = c.Max[i]
		}
		s.N[i] += c.N[i]
	}
	for k, v := range c.Errors {
		s.Errors[k] += v
	}
}

func (s Stats) Percentiles(eventType byte, p []float64) (q []uint64) {
	if len(p) == 0 {
		return []uint64{}
	}
	q = make([]uint64, len(p)) // returned ^ approximate percentiles
	n := uint64(0)             // running total event count
	f := 0.0                   // running total frequency (percentile per bucket)
	j := 0                     // index in p[] and q[]
	for i := range s.Buckets[eventType] {
		n += s.Buckets[eventType][i]
		f = float64(n) / float64(s.N[eventType]) * 100
		// i    f     p[j]   Bucket[i]
		// ---  ----  -----  -----------------
		// 100  94.1         500100 (500.1 ms)
		// 101  95.7         502300 (502.3 ms)
		// 102  96.0  95.0
		//fmt.Printf("at %d n = %d, f = %f, p = %f ap = %f\n", i, n, f, p[j], base*math.Pow(factor, float64(i)))
		if f >= p[j] {
			q[j] = uint64(base * math.Pow(factor, float64(i))) // high value

			intP, _ := math.Modf(p[j])
			intF, _ := math.Modf(f)
			if intP != intF && i > 0 {
				// Take midpoint value between two whole percentiles like P45<->P60
				// to obtain a more accurate P between them, like P50 in this example.
				// q[j] is already high value, so get low value to calc midpoint.
				lowValue := uint64(base * math.Pow(factor, float64(i-1)))
				q[j] = (q[j] + lowValue) / 2 // midpoint
			} else if i == 0 {
				q[j] = uint64(base * math.Pow(factor, 0.0)) // first, lowest bucket
			}
			if j == len(q)-1 { // stop early once all p[] found
				break
			}
			j++ // find next p[]
		}
	}
	return // q
}

func ParsePercentiles(pCSV string) ([]string, []float64, error) {
	if strings.TrimSpace(pCSV) == "" {
		return DefaultPercentileNames, DefaultPercentiles, nil
	}
	all := strings.Split(pCSV, ",")
	if len(all) == 0 {
		return nil, nil, nil
	}
	s := []string{}  // name "P99.9"
	p := []float64{} // value 99.9
	for _, raw := range all {
		pStr := strings.TrimLeft(strings.TrimSpace(raw), "Pp") // p99 -> 99
		f, err := strconv.ParseFloat(pStr, 64)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid percentile: %s: %s", pStr, err)
		}
		if f < 0.0 || f > 100.0 {
			return nil, nil, fmt.Errorf("percentile out of range: %s (%f): must be bretween 0 and 100", pStr, f)
		}
		s = append(s, "P"+pStr) // 99 -> P99 (string)
		p = append(p, f)        // 99.0 (float)
	}
	return s, p, nil
}

// --------------------------------------------------------------------------

// Trx is lock-free stats for one trx file by one client. It contains 2 pre-allocated
// Stats struct called "a" and "b": one active, one reporting. A Client records
// to the active stats. When the Collector is ready to collect stats, it calls
// Swap, and Trx atomically swaps "a" and "b". If, for example, "a" was active,
// then it's returned to the Collector for reporting, and "b" is made active for
// on-going stats recording by the Client. This is the other half of the lock-free
// Stats design.
type Trx struct {
	Name string
	a    *Stats
	b    *Stats
	sp   atomic.Pointer[Stats]
	onA  bool
}

func NewTrx(name string) *Trx {
	a := NewStats()
	b := NewStats()
	sp := atomic.Pointer[Stats]{}
	sp.Store(a)
	return &Trx{
		Name: name,
		sp:   sp,
		a:    a,
		b:    b,
		onA:  true,
	}
}

func (t *Trx) Record(eventType byte, d int64) {
	t.sp.Load().Record(eventType, d)
}

func (t *Trx) Error(n uint16) {
	t.sp.Load().Errors[n] += 1
}

func (t *Trx) Swap() *Stats {
	// on A; switch to B
	if t.onA {
		t.b.Reset()
		t.sp.Store(t.b)
		t.onA = false
		return t.a
	}
	// on B; switch to A
	t.a.Reset()
	t.sp.Store(t.a)
	t.onA = true
	return t.b
}

/*
  0 [0.000000, 10.000000)		10 us
  1 [10.000000, 10.471285)
  2 [10.471285, 10.964782)
  3 [10.964782, 11.481536)
  4 [11.481536, 12.022644)
  5 [12.022644, 12.589254)
  6 [12.589254, 13.182567)
  7 [13.182567, 13.803843)
  8 [13.803843, 14.454398)
  9 [14.454398, 15.135612)
 10 [15.135612, 15.848932)
 11 [15.848932, 16.595869)
 12 [16.595869, 17.378008)
 13 [17.378008, 18.197009)
 14 [18.197009, 19.054607)
 15 [19.054607, 19.952623)
 16 [19.952623, 20.892961)
 17 [20.892961, 21.877616)
 18 [21.877616, 22.908677)
 19 [22.908677, 23.988329)
 20 [23.988329, 25.118864)
 21 [25.118864, 26.302680)
 22 [26.302680, 27.542287)
 23 [27.542287, 28.840315)
 24 [28.840315, 30.199517)
 25 [30.199517, 31.622777)
 26 [31.622777, 33.113112)
 27 [33.113112, 34.673685)
 28 [34.673685, 36.307805)
 29 [36.307805, 38.018940)
 30 [38.018940, 39.810717)
 31 [39.810717, 41.686938)
 32 [41.686938, 43.651583)
 33 [43.651583, 45.708819)
 34 [45.708819, 47.863009)
 35 [47.863009, 50.118723)
 36 [50.118723, 52.480746)
 37 [52.480746, 54.954087)
 38 [54.954087, 57.543994)
 39 [57.543994, 60.255959)
 40 [60.255959, 63.095734)
 41 [63.095734, 66.069345)
 42 [66.069345, 69.183097)
 43 [69.183097, 72.443596)
 44 [72.443596, 75.857758)
 45 [75.857758, 79.432823)
 46 [79.432823, 83.176377)
 47 [83.176377, 87.096359)
 48 [87.096359, 91.201084)
 49 [91.201084, 95.499259)
 50 [95.499259, 100.000000)
 51 [100.000000, 104.712855)	100 us
 52 [104.712855, 109.647820)
 53 [109.647820, 114.815362)
 54 [114.815362, 120.226443)
 55 [120.226443, 125.892541)
 56 [125.892541, 131.825674)
 57 [131.825674, 138.038426)
 58 [138.038426, 144.543977)
 59 [144.543977, 151.356125)
 60 [151.356125, 158.489319)
 61 [158.489319, 165.958691)
 62 [165.958691, 173.780083)
 63 [173.780083, 181.970086)
 64 [181.970086, 190.546072)
 65 [190.546072, 199.526231)
 66 [199.526231, 208.929613)
 67 [208.929613, 218.776162)
 68 [218.776162, 229.086765)
 69 [229.086765, 239.883292)
 70 [239.883292, 251.188643)
 71 [251.188643, 263.026799)
 72 [263.026799, 275.422870)
 73 [275.422870, 288.403150)
 74 [288.403150, 301.995172)
 75 [301.995172, 316.227766)
 76 [316.227766, 331.131121)
 77 [331.131121, 346.736850)
 78 [346.736850, 363.078055)
 79 [363.078055, 380.189396)
 80 [380.189396, 398.107171)
 81 [398.107171, 416.869383)
 82 [416.869383, 436.515832)
 83 [436.515832, 457.088190)
 84 [457.088190, 478.630092)
 85 [478.630092, 501.187234)
 86 [501.187234, 524.807460)
 87 [524.807460, 549.540874)
 88 [549.540874, 575.439937)
 89 [575.439937, 602.559586)
 90 [602.559586, 630.957344)
 91 [630.957344, 660.693448)
 92 [660.693448, 691.830971)
 93 [691.830971, 724.435960)
 94 [724.435960, 758.577575)
 95 [758.577575, 794.328235)
 96 [794.328235, 831.763771)
 97 [831.763771, 870.963590)
 98 [870.963590, 912.010839)
 99 [912.010839, 954.992586)
100 [954.992586, 1000.000000)
101 [1000.000000, 1047.128548)	1 ms
102 [1047.128548, 1096.478196)
103 [1096.478196, 1148.153621)
104 [1148.153621, 1202.264435)
105 [1202.264435, 1258.925412)
106 [1258.925412, 1318.256739)
107 [1318.256739, 1380.384265)
108 [1380.384265, 1445.439771)
109 [1445.439771, 1513.561248)
110 [1513.561248, 1584.893192)
111 [1584.893192, 1659.586907)
112 [1659.586907, 1737.800829)
113 [1737.800829, 1819.700859)
114 [1819.700859, 1905.460718)
115 [1905.460718, 1995.262315)
116 [1995.262315, 2089.296131)
117 [2089.296131, 2187.761624)
118 [2187.761624, 2290.867653)
119 [2290.867653, 2398.832919)
120 [2398.832919, 2511.886432)
121 [2511.886432, 2630.267992)
122 [2630.267992, 2754.228703)
123 [2754.228703, 2884.031503)
124 [2884.031503, 3019.951720)
125 [3019.951720, 3162.277660)
126 [3162.277660, 3311.311215)
127 [3311.311215, 3467.368505)
128 [3467.368505, 3630.780548)
129 [3630.780548, 3801.893963)
130 [3801.893963, 3981.071706)
131 [3981.071706, 4168.693835)
132 [4168.693835, 4365.158322)
133 [4365.158322, 4570.881896)
134 [4570.881896, 4786.300923)
135 [4786.300923, 5011.872336)
136 [5011.872336, 5248.074602)
137 [5248.074602, 5495.408739)
138 [5495.408739, 5754.399373)
139 [5754.399373, 6025.595861)
140 [6025.595861, 6309.573445)
141 [6309.573445, 6606.934480)
142 [6606.934480, 6918.309709)
143 [6918.309709, 7244.359601)
144 [7244.359601, 7585.775750)
145 [7585.775750, 7943.282347)
146 [7943.282347, 8317.637711)
147 [8317.637711, 8709.635900)
148 [8709.635900, 9120.108394)
149 [9120.108394, 9549.925860)
150 [9549.925860, 10000.000000)
151 [10000.000000, 10471.285481)	10 ms
152 [10471.285481, 10964.781961)
153 [10964.781961, 11481.536215)
154 [11481.536215, 12022.644346)
155 [12022.644346, 12589.254118)
156 [12589.254118, 13182.567386)
157 [13182.567386, 13803.842646)
158 [13803.842646, 14454.397707)
159 [14454.397707, 15135.612484)
160 [15135.612484, 15848.931925)
161 [15848.931925, 16595.869074)
162 [16595.869074, 17378.008287)
163 [17378.008287, 18197.008586)
164 [18197.008586, 19054.607180)
165 [19054.607180, 19952.623150)
166 [19952.623150, 20892.961309)
167 [20892.961309, 21877.616239)
168 [21877.616239, 22908.676528)
169 [22908.676528, 23988.329190)
170 [23988.329190, 25118.864315)
171 [25118.864315, 26302.679919)
172 [26302.679919, 27542.287033)
173 [27542.287033, 28840.315031)
174 [28840.315031, 30199.517204)
175 [30199.517204, 31622.776602)
176 [31622.776602, 33113.112148)
177 [33113.112148, 34673.685045)
178 [34673.685045, 36307.805477)
179 [36307.805477, 38018.939632)
180 [38018.939632, 39810.717055)
181 [39810.717055, 41686.938347)
182 [41686.938347, 43651.583224)
183 [43651.583224, 45708.818961)
184 [45708.818961, 47863.009232)
185 [47863.009232, 50118.723363)
186 [50118.723363, 52480.746025)
187 [52480.746025, 54954.087386)
188 [54954.087386, 57543.993734)
189 [57543.993734, 60255.958607)
190 [60255.958607, 63095.734448)
191 [63095.734448, 66069.344801)
192 [66069.344801, 69183.097092)
193 [69183.097092, 72443.596007)
194 [72443.596007, 75857.757503)
195 [75857.757503, 79432.823472)
196 [79432.823472, 83176.377110)
197 [83176.377110, 87096.358996)
198 [87096.358996, 91201.083936)
199 [91201.083936, 95499.258602)
200 [95499.258602, 100000.000000)
201 [100000.000000, 104712.854805)	100 ms
202 [104712.854805, 109647.819614)
203 [109647.819614, 114815.362150)
204 [114815.362150, 120226.443462)
205 [120226.443462, 125892.541179)
206 [125892.541179, 131825.673856)
207 [131825.673856, 138038.426460)
208 [138038.426460, 144543.977075)
209 [144543.977075, 151356.124844)
210 [151356.124844, 158489.319246)
211 [158489.319246, 165958.690744)
212 [165958.690744, 173780.082875)
213 [173780.082875, 181970.085861)
214 [181970.085861, 190546.071796)
215 [190546.071796, 199526.231497)
216 [199526.231497, 208929.613085)
217 [208929.613085, 218776.162395)
218 [218776.162395, 229086.765277)
219 [229086.765277, 239883.291902)
220 [239883.291902, 251188.643151)
221 [251188.643151, 263026.799190)
222 [263026.799190, 275422.870334)
223 [275422.870334, 288403.150313)
224 [288403.150313, 301995.172040)
225 [301995.172040, 316227.766017)
226 [316227.766017, 331131.121483)
227 [331131.121483, 346736.850453)
228 [346736.850453, 363078.054770)
229 [363078.054770, 380189.396321)
230 [380189.396321, 398107.170554)
231 [398107.170554, 416869.383470)
232 [416869.383470, 436515.832240)
233 [436515.832240, 457088.189615)
234 [457088.189615, 478630.092323)
235 [478630.092323, 501187.233627)
236 [501187.233627, 524807.460250)
237 [524807.460250, 549540.873858)
238 [549540.873858, 575439.937337)
239 [575439.937337, 602559.586074)
240 [602559.586074, 630957.344480)
241 [630957.344480, 660693.448008)
242 [660693.448008, 691830.970919)
243 [691830.970919, 724435.960075)
244 [724435.960075, 758577.575029)
245 [758577.575029, 794328.234724)
246 [794328.234724, 831763.771103)
247 [831763.771103, 870963.589956)
248 [870963.589956, 912010.839356)
249 [912010.839356, 954992.586021)
250 [954992.586021, 1000000.000000)
251 [1000000.000000, 1047128.548051)	1 s
252 [1047128.548051, 1096478.196143)
253 [1096478.196143, 1148153.621497)
254 [1148153.621497, 1202264.434617)
255 [1202264.434617, 1258925.411794)
256 [1258925.411794, 1318256.738556)
257 [1318256.738556, 1380384.264603)
258 [1380384.264603, 1445439.770746)
259 [1445439.770746, 1513561.248436)
260 [1513561.248436, 1584893.192461)
261 [1584893.192461, 1659586.907438)
262 [1659586.907438, 1737800.828749)
263 [1737800.828749, 1819700.858610)
264 [1819700.858610, 1905460.717963)
265 [1905460.717963, 1995262.314969)
266 [1995262.314969, 2089296.130854)
267 [2089296.130854, 2187761.623950)
268 [2187761.623950, 2290867.652768)
269 [2290867.652768, 2398832.919020)
270 [2398832.919020, 2511886.431510)
271 [2511886.431510, 2630267.991895)
272 [2630267.991895, 2754228.703338)
273 [2754228.703338, 2884031.503127)
274 [2884031.503127, 3019951.720402)
275 [3019951.720402, 3162277.660168)
276 [3162277.660168, 3311311.214826)
277 [3311311.214826, 3467368.504525)
278 [3467368.504525, 3630780.547701)
279 [3630780.547701, 3801893.963206)
280 [3801893.963206, 3981071.705535)
281 [3981071.705535, 4168693.834703)
282 [4168693.834703, 4365158.322402)
283 [4365158.322402, 4570881.896149)
284 [4570881.896149, 4786300.923226)
285 [4786300.923226, 5011872.336273)
286 [5011872.336273, 5248074.602498)
287 [5248074.602498, 5495408.738576)
288 [5495408.738576, 5754399.373372)
289 [5754399.373372, 6025595.860744)
290 [6025595.860744, 6309573.444802)
291 [6309573.444802, 6606934.480076)
292 [6606934.480076, 6918309.709189)
293 [6918309.709189, 7244359.600750)
294 [7244359.600750, 7585775.750292)
295 [7585775.750292, 7943282.347243)
296 [7943282.347243, 8317637.711027)
297 [8317637.711027, 8709635.899561)
298 [8709635.899561, 9120108.393559)
299 [9120108.393559, 9549925.860215)
300 [9549925.860215, 10000000.000000)
301 [10000000.000000, 10471285.480509)
302 [10471285.480509, 10964781.961432)
303 [10964781.961432, 11481536.214969)
304 [11481536.214969, 12022644.346174)
305 [12022644.346174, 12589254.117942)
306 [12589254.117942, 13182567.385564)
307 [13182567.385564, 13803842.646029)
308 [13803842.646029, 14454397.707460)
309 [14454397.707460, 15135612.484362)
310 [15135612.484362, 15848931.924611)
311 [15848931.924611, 16595869.074376)
312 [16595869.074376, 17378008.287494)
313 [17378008.287494, 18197008.586100)
314 [18197008.586100, 19054607.179633)
315 [19054607.179633, 19952623.149689)
316 [19952623.149689, 20892961.308541)
317 [20892961.308541, 21877616.239496)
318 [21877616.239496, 22908676.527678)
319 [22908676.527678, 23988329.190195)
320 [23988329.190195, 25118864.315096)
321 [25118864.315096, 26302679.918954)
322 [26302679.918954, 27542287.033382)
323 [27542287.033382, 28840315.031267)
324 [28840315.031267, 30199517.204021)
325 [30199517.204021, 31622776.601684)
326 [31622776.601684, 33113112.148260)
327 [33113112.148260, 34673685.045254)
328 [34673685.045254, 36307805.477011)
329 [36307805.477011, 38018939.632057)
330 [38018939.632057, 39810717.055350)
331 [39810717.055350, 41686938.347034)
332 [41686938.347034, 43651583.224017)
333 [43651583.224017, 45708818.961488)
334 [45708818.961488, 47863009.232265)
335 [47863009.232265, 50118723.362728)
336 [50118723.362728, 52480746.024978)
337 [52480746.024978, 54954087.385764)
338 [54954087.385764, 57543993.733717)
339 [57543993.733717, 60255958.607437)
340 [60255958.607437, 63095734.448021)
341 [63095734.448021, 66069344.800761)
342 [66069344.800761, 69183097.091895)
343 [69183097.091895, 72443596.007500)
344 [72443596.007500, 75857757.502920)
345 [75857757.502920, 79432823.472430)
346 [79432823.472430, 83176377.110269)
347 [83176377.110269, 87096358.995610)
348 [87096358.995610, 91201083.935593)
349 [91201083.935593, 95499258.602146)
350 [95499258.602146, 100000000.000002)
351 [100000000.000002, 104712854.805092)
352 [104712854.805092, 109647819.614321)
353 [109647819.614321, 114815362.149691)
354 [114815362.149691, 120226443.461744)
355 [120226443.461744, 125892541.179419)
356 [125892541.179419, 131825673.855643)
357 [131825673.855643, 138038426.460291)
358 [138038426.460291, 144543977.074596)
359 [144543977.074596, 151356124.843624)
360 [151356124.843624, 158489319.246115)
361 [158489319.246115, 165958690.743760)
362 [165958690.743760, 173780082.874941)
363 [173780082.874941, 181970085.861002)
364 [181970085.861002, 190546071.796329)
365 [190546071.796329, 199526231.496892)
366 [199526231.496892, 208929613.085408)
367 [208929613.085408, 218776162.394960)
368 [218776162.394960, 229086765.276782)
369 [229086765.276782, 239883291.901954)
370 [239883291.901954, 251188643.150963)
371 [251188643.150963, 263026799.189544)
372 [263026799.189544, 275422870.333823)
373 [275422870.333823, 288403150.312667)
374 [288403150.312667, 301995172.040208)
375 [301995172.040208, 316227766.016845)
376 [316227766.016845, 331131121.482598)
377 [331131121.482598, 346736850.452539)
378 [346736850.452539, 363078054.770109)
379 [363078054.770109, 380189396.320570)
380 [380189396.320570, 398107170.553506)
381 [398107170.553506, 416869383.470345)
382 [416869383.470345, 436515832.240176)
383 [436515832.240176, 457088189.614885)
384 [457088189.614885, 478630092.322649)
385 [478630092.322649, 501187233.627284)
386 [501187233.627284, 524807460.249785)
387 [524807460.249785, 549540873.857637)
388 [549540873.857637, 575439937.337170)
389 [575439937.337170, 602559586.074371)
390 [602559586.074371, 630957344.480208)
391 [630957344.480208, 660693448.007611)
392 [660693448.007611, 691830970.918952)
393 [691830970.918952, 724435960.075007)
394 [724435960.075007, 758577575.029201)
395 [758577575.029201, 794328234.724300)
396 [794328234.724300, 831763771.102690)
397 [831763771.102690, 870963589.956101)
398 [870963589.956101, 912010839.355931)
399 [912010839.355931, 954992586.021458)
400 [954992586.021458, 1000000000.000024)
401 [1000000000.000024, 1047128548.050924)
402 [1047128548.050924, 1096478196.143211)
403 [1096478196.143211, 1148153621.496910)
404 [1148153621.496910, 1202264434.617441)
405 [1202264434.617441, 1258925411.794197)
406 [1258925411.794197, 1318256738.556438)
407 [1318256738.556438, 1380384264.602918)
408 [1380384264.602918, 1445439770.745962)
409 [1445439770.745962, 1513561248.436245)
410 [1513561248.436245, 1584893192.461152)
411 [1584893192.461152, 1659586907.437601)
412 [1659586907.437601, 1737800828.749417)
413 [1737800828.749417, 1819700858.610027)
414 [1819700858.610027, 1905460717.963293)
415 [1905460717.963293, 1995262314.968928)
416 [1995262314.968928, 2089296130.854091)
417 [2089296130.854091, 2187761623.949606)
418 [2187761623.949606, 2290867652.767829)
419 [2290867652.767829, 2398832919.019549)
420 [2398832919.019549, 2511886431.509642)
421 [2511886431.509642, 2630267991.895447)
422 [2630267991.895447, 2754228703.338235)
423 [2754228703.338235, 2884031503.126678)
424 [2884031503.126678, 3019951720.402092)
425 [3019951720.402092, 3162277660.168459)
426 [3162277660.168459, 3311311214.825994)
427 [3311311214.825994, 3467368504.525403)
428 [3467368504.525403, 3630780547.701105)
429 [3630780547.701105, 3801893963.205708)
430 [3801893963.205708, 3981071705.535073)
431 [3981071705.535073, 4168693834.703460)
432 [4168693834.703460, 4365158322.401771)
433 [4365158322.401771, 4570881896.148867)
434 [4570881896.148867, 4786300923.226506)
435 [4786300923.226506, 5011872336.272851)
436 [5011872336.272851, 5248074602.497861)
437 [5248074602.497861, 5495408738.576387)
438 [5495408738.576387, 5754399373.371716)
439 [5754399373.371716, 6025595860.743733)
440 [6025595860.743733, 6309573444.802095)
441 [6309573444.802095, 6606934480.076132)
442 [6606934480.076132, 6918309709.189544)
443 [6918309709.189544, 7244359600.750089)
444 [7244359600.750089, 7585775750.292035)
445 [7585775750.292035, 7943282347.243022)
446 [7943282347.243022, 8317637711.026927)
447 [8317637711.026927, 8709635899.561033)
448 [8709635899.561033, 9120108393.559340)
449 [9120108393.559340, 9549925860.214613)
*/
