package pgtest

import "testing"

func TestIsDisposableTestDatabase(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"postgres://postgres:postgres@localhost:5432/zl_expense?sslmode=disable", false},
		{"postgres://postgres:postgres@localhost:5432/zl_expense_prod?sslmode=disable", false},
		{"postgres://postgres:postgres@localhost:5432/zl_expense_test?sslmode=disable", true},
		{"postgres://postgres:postgres@localhost:5432/app_test", true},
	}
	for _, tc := range cases {
		name, err := DatabaseName(tc.url)
		if err != nil {
			t.Fatalf("%s: %v", tc.url, err)
		}
		if got := IsDisposableTestDatabase(name); got != tc.want {
			t.Fatalf("%s: name=%q got %v want %v", tc.url, name, got, tc.want)
		}
	}
}
