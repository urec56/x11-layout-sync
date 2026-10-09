package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Xauthority file format (see libXau AuRead.c): a sequence of records
//
//	family:    CARD16 (big endian)
//	address:   counted string (host)
//	number:    counted string (display number)
//	name:      counted string (auth protocol name)
//	data:      counted string (auth data)
//
// Matching follows XauGetBestAuthByAddr for a local connection:
// family 0xffff (FamilyWild) matches anything; otherwise the record must
// be family 1 (FamilyLocal) with an empty address; the display number must
// match (or be empty in the record).

const (
	familyLocal = 1
	familyWild  = 0xffff
)

func readXAuthority(display string) (authName string, authData []byte, ok bool) {
	path := os.Getenv("XAUTHORITY")
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", nil, false
		}
		path = filepath.Join(home, ".Xauthority")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", nil, false
	}

	wantNum := strconv.Itoa(displayNum(display))
	i := 0
	for i+2 <= len(content) {
		family := be16(content[i : i+2])
		i += 2

		readStr := func() (string, bool) {
			if i+2 > len(content) {
				return "", false
			}
			n := int(be16(content[i : i+2]))
			i += 2
			if n < 0 || i+n > len(content) {
				return "", false
			}
			s := string(content[i : i+n])
			i += n
			return s, true
		}

		addr, ok1 := readStr()
		number, ok2 := readStr()
		name, ok3 := readStr()
		data, ok4 := readStr()
		if !ok1 || !ok2 || !ok3 || !ok4 {
			break
		}

		familyOK := family == familyWild || (family == familyLocal && addr == "")
		numberOK := number == "" || number == wantNum
		if name == "MIT-MAGIC-COOKIE-1" && familyOK && numberOK {
			return name, []byte(data), true
		}
	}
	return "", nil, false
}

// displayNum extracts the numeric display number from a display string.
func displayNum(d string) int {
	if i := strings.LastIndexByte(d, ':'); i >= 0 {
		d = d[i+1:]
	}
	if i := strings.IndexByte(d, '.'); i >= 0 {
		d = d[:i]
	}
	n, _ := strconv.Atoi(d)
	return n
}
