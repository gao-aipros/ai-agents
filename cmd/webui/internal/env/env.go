package env

import (
	"os"
	"strconv"
)

// String returns the value of the environment variable named by key,
// or def if the variable is not set or empty.
func String(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// Int returns the value of the environment variable named by key as an int,
// or def if the variable is not set, empty, or not a valid integer.
func Int(key string, def int) int {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}
