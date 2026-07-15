package main

import "testing"

func TestDefaultHerdrSocketUsesConfigDirectory(t *testing.T) {
	t.Setenv("HOME", "/home/herdr")
	if got, want := defaultHerdrSocket(), "/home/herdr/.config/herdr/herdr.sock"; got != want {
		t.Fatalf("default Herdr socket = %q, want %q", got, want)
	}
}
