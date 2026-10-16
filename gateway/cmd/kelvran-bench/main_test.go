package main

import "testing"

func TestMetricsURLFlagIsRepeatable(t *testing.T) {
	var u urlsFlag
	for _, v := range []string{"http://127.0.0.1:9464/metrics", "http://127.0.0.1:9465/metrics"} {
		if err := u.Set(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(u) != 2 || u[0] != "http://127.0.0.1:9464/metrics" || u[1] != "http://127.0.0.1:9465/metrics" {
		t.Errorf("urls = %v", u)
	}
	if err := u.Set(""); err == nil {
		t.Error("an empty URL must be refused")
	}
	if err := u.Set("not a url"); err == nil {
		t.Error("a value without a scheme must be refused")
	}
	if err := u.Set("ftp://127.0.0.1/metrics"); err == nil {
		t.Error("only http and https are metrics endpoints")
	}
}
