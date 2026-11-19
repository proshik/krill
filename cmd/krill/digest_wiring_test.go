package main

import (
	"testing"

	"github.com/proshik/krill/internal/docker"
	"github.com/proshik/krill/internal/notify"
)

// main finds the digest's Swarm reader by a type assertion on the engine and
// goes without it — no crashes, no stopped/down apps — when it fails.
func TestEngineIsDigestSwarmReader(t *testing.T) {
	eng, err := docker.NewEngine("unix:///nonexistent/docker.sock") // does not connect
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := eng.(notify.SwarmReader); !ok {
		t.Fatal("the docker engine no longer implements notify.SwarmReader")
	}
}
