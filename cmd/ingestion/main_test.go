package main

import "testing"

func TestBearerToken(t *testing.T) {
	tests := map[string]struct {
		headers map[string]string
		want    string
	}{
		"standard header":       {map[string]string{"Authorization": "Bearer abc.def.ghi"}, "abc.def.ghi"},
		"lower-cased key":       {map[string]string{"authorization": "Bearer abc.def.ghi"}, "abc.def.ghi"},
		"no auth header":        {map[string]string{}, ""},
		"non-bearer scheme":     {map[string]string{"Authorization": "Basic dXNlcjpwYXNz"}, ""},
		"empty bearer token":    {map[string]string{"Authorization": "Bearer "}, ""},
		"missing bearer prefix": {map[string]string{"Authorization": "abc.def.ghi"}, ""},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			if got := bearerToken(tc.headers); got != tc.want {
				t.Errorf("bearerToken(%v) = %q, want %q", tc.headers, got, tc.want)
			}
		})
	}
}
