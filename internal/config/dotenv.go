package config

import (
	"bufio"
	"fmt"
	"os"
	"strings"
)

// LoadDotEnv reads a .env file and sets every key it names, unless the key is
// already set in the real environment.
//
// The precedence is deliberate and matches what operators expect from docker
// compose and systemd: a value exported in the process environment wins over the
// file, so dropping a .env into a container never silently overrides a secret
// that was injected by the platform.
//
// A missing file is not an error. Callers pass the path explicitly, so an
// optional .env is spelled ".env" and a required one is checked by the caller.
//
// The syntax is the small, widely used subset:
//
//	KEY=value             unquoted, trailing spaces trimmed
//	KEY="value"           double quotes; \n \t \" \\ and nothing else escapes
//	KEY='value'           single quotes; taken literally
//	# comment             full-line comments only
//	export KEY=value      the export prefix is accepted and ignored
//
// Full-line comments only: a "#" inside a value is part of that value, because a
// cookie or token can legitimately contain one.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	// A cookie or token can be long; the default 64KB line is enough, but the
	// buffer is raised anyway so a wrapped secret does not fail with a confusing
	// "token too long".
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		key, val, ok := splitDotEnv(text)
		if !ok {
			return fmt.Errorf("%s:%d: expected KEY=value", path, line)
		}
		// Real environment wins.
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, val); err != nil {
			return err
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

// splitDotEnv parses one line into a key and value.
func splitDotEnv(text string) (string, string, bool) {
	text = strings.TrimPrefix(text, "export ")
	text = strings.TrimSpace(text)

	eq := strings.IndexByte(text, '=')
	if eq <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(text[:eq])
	raw := strings.TrimSpace(text[eq+1:])
	if key == "" {
		return "", "", false
	}

	// A quoted value keeps everything between the quotes, including "#".
	if len(raw) >= 2 {
		switch raw[0] {
		case '"':
			if raw[len(raw)-1] == '"' {
				return key, unescapeDouble(raw[1 : len(raw)-1]), true
			}
		case '\'':
			if raw[len(raw)-1] == '\'' {
				return key, raw[1 : len(raw)-1], true
			}
		}
	}
	return key, raw, true
}

// unescapeDouble handles the escapes a quoted .env value needs. An unrecognised
// escape is kept as-is rather than dropped, so a Windows path or a regex in a
// value survives.
func unescapeDouble(s string) string {
	if !strings.ContainsRune(s, '\\') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		i++
		switch s[i] {
		case 'n':
			b.WriteByte('\n')
		case 't':
			b.WriteByte('\t')
		case 'r':
			b.WriteByte('\r')
		case '"':
			b.WriteByte('"')
		case '\\':
			b.WriteByte('\\')
		default:
			b.WriteByte('\\')
			b.WriteByte(s[i])
		}
	}
	return b.String()
}
