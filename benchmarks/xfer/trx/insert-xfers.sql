
-- prepare
-- table-size: xfers $params.xfers-size
INSERT INTO xfers VALUES /*!csv 500 (NULL, @x_token, 0, 'USD', @s_token, @r_token, 1, @c1, @c2, @c3, NULL, NULL, NULL, 0, 0, NOW(), NOW()) */
