package server

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
)

// newID returns a 128-bit random hex identifier.
func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// parseVersion reads the {version} path value as a positive integer.
func parseVersion(r *http.Request) (int, error) {
	raw := r.PathValue("version")
	v, err := strconv.Atoi(raw)
	if err != nil || v <= 0 {
		return 0, errors.New("version must be a positive integer")
	}
	return v, nil
}

// itoa formats an integer.
func itoa(i int) string {
	return strconv.Itoa(i)
}

func health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
