package api

import (
	"strings"
	"testing"
)

func TestBuildShareRedirectTarget(t *testing.T) {
	cases := []struct {
		name string
		path string
		code string
		want string
	}{
		{
			name: "share param lands before the hash fragment",
			path: "/web/#/test-runs/1783062934972/chromium%20%E2%80%BA%20ontology%2Fsmoke.spec.ts",
			code: "abcDEF123-",
			want: "/web/?share=abcDEF123-#/test-runs/1783062934972/chromium%20%E2%80%BA%20ontology%2Fsmoke.spec.ts",
		},
		{
			name: "no fragment",
			path: "/web/",
			code: "abc",
			want: "/web/?share=abc",
		},
		{
			name: "existing query gets ampersand",
			path: "/web/?theme=dark#/test-runs/1",
			code: "abc",
			want: "/web/?theme=dark&share=abc#/test-runs/1",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildShareRedirectTarget(tc.path, tc.code); got != tc.want {
				t.Errorf("buildShareRedirectTarget(%q, %q) = %q, want %q", tc.path, tc.code, got, tc.want)
			}
		})
	}
}

func TestGenerateShareCode(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		code, err := generateShareCode(shareCodeLength)
		if err != nil {
			t.Fatalf("generateShareCode: %v", err)
		}
		if len(code) != shareCodeLength {
			t.Fatalf("code length = %d, want %d", len(code), shareCodeLength)
		}
		for _, r := range code {
			if !strings.ContainsRune(shareCodeAlphabet, r) {
				t.Fatalf("code %q contains %q outside the URL-safe alphabet", code, r)
			}
		}
		if seen[code] {
			t.Fatalf("duplicate code %q in 100 draws", code)
		}
		seen[code] = true
	}
}
