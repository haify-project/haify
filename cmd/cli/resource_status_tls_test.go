package main

import (
	"testing"

	haifypb "github.com/haify-project/haify/api/proto/v1"
	"github.com/stretchr/testify/assert"
)

func TestReplicationEncryption(t *testing.T) {
	self := &haifypb.NodeResourceState{Role: "Primary", DiskState: "UpToDate"}
	tlsPeer := &haifypb.NodeResourceState{ReplicationState: "Established", Tls: true}
	plainPeer := &haifypb.NodeResourceState{ReplicationState: "Established"}

	assert.Contains(t, replicationEncryption(map[string]*haifypb.NodeResourceState{"a": self, "b": tlsPeer, "c": tlsPeer}),
		"is encrypted (kernel TLS on every connection)")
	assert.Contains(t, replicationEncryption(map[string]*haifypb.NodeResourceState{"a": self, "b": tlsPeer, "c": plainPeer}),
		"on 1 of 2 connections only")
	assert.Contains(t, replicationEncryption(map[string]*haifypb.NodeResourceState{"a": self, "b": plainPeer}), "NOT encrypted")
	assert.Contains(t, replicationEncryption(nil), "NOT encrypted")
}
