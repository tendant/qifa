package secrets

import (
	"bytes"
	"fmt"
	"os"
	"sort"
	"strings"
)

func Render(clear map[string]string, secretKeys []string) ([]byte, error) {
	values := map[string]string{}
	for key, value := range clear {
		values[key] = value
	}
	for _, key := range secretKeys {
		value, ok := os.LookupEnv(key)
		if !ok {
			return nil, fmt.Errorf("required secret env %s is not set", key)
		}
		values[key] = value
	}

	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	for _, key := range keys {
		fmt.Fprintf(&buf, "%s=%s\n", key, envFileEscape(values[key]))
	}
	return buf.Bytes(), nil
}

func envFileEscape(value string) string {
	return strings.ReplaceAll(value, "\n", "\\n")
}
