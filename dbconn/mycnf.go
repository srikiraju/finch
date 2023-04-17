// Copyright 2023 Block, Inc.

package dbconn

import (
	"strings"

	"github.com/go-ini/ini"

	"github.com/square/finch"
	"github.com/square/finch/config"
)

// ParseMyCnf parses a MySQL my.cnf file. It only reads the "[client]" section,
// same as the mysql CLI.
func ParseMyCnf(file string) (config.MySQL, error) {
	opts := ini.LoadOptions{AllowBooleanKeys: true}
	mycnf, err := ini.LoadSources(opts, file)
	if err != nil {
		return config.MySQL{}, err
	}

	cfg := config.MySQL{
		Username: mycnf.Section("client").Key("user").String(),
		Password: mycnf.Section("client").Key("password").String(),
		Hostname: mycnf.Section("client").Key("host").String(),
		Socket:   mycnf.Section("client").Key("socket").String(),
	}

	port := mycnf.Section("client").Key("port").String()
	if port != "" {
		cfg.Hostname += ":" + port
	}

	// Translate MySQL ssl-* vars to config.TLS. The vars don't line up
	// perfectly because MySQL has several levels of TLS verification:
	//   https://dev.mysql.com/doc/refman/8.0/en/connection-options.html#option_general_ssl-mode
	// But Go tls.Config (which is derived from config.TLS) has only two
	// options: specify tls.Confg.ServerName _or_ .InsecureSkipVerify=true.
	mysqlTLS(file, mycnf, &cfg)

	finch.Debug("mycnf %s: %s %+v", file, cfg.Redacted())
	return cfg, nil
}

func mysqlTLS(file string, mycnf *ini.File, cfg *config.MySQL) (tls config.TLS) {
	// USING IMPLICIT RETURN -----------------------------------^

	tls.MySQLMode = strings.ToUpper(mycnf.Section("client").Key("ssl-mode").String())
	if tls.MySQLMode == "" {
		tls.MySQLMode = "PREFERRED" // MySQL default
	}

	// Explicitly disabled = not TLS even if other vars set
	if tls.MySQLMode == "DISABLED" {
		finch.Debug("mycnf %s: ssl-mode=DISABLED", file)
		return
	}

	// As per the MySQL manual:
	// "Connections over Unix socket files are not encrypted with a mode of PREFERRED.
	//  To enforce encryption for Unix socket-file connections, use a mode of REQUIRED or stricter.
	if cfg.Socket != "" && tls.MySQLMode == "PREFERRED" {
		finch.Debug("mycnf %s: ignoring TLS on socket %s", file, cfg.Socket)
		return
	}

	// Not TLS unless at least 1 of the 3 files is set (no validation yet)
	tls.CA = mycnf.Section("client").Key("ssl-ca").String()
	tls.Cert = mycnf.Section("client").Key("ssl-cert").String()
	tls.Key = mycnf.Section("client").Key("ssl-key").String()
	if !tls.Set() {
		finch.Debug("mycnf %s: TLS not set", file)
		return
	}

	// Probably legit/normal MySQL TLS config: hostname + at least 1 file.
	// But it's unclear if, for example, PREFERRED = SkipVerify=true?
	return
}
