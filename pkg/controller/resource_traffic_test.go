package controller

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Two volumes, two peers: the answering node's device counters and each
// peer's sent/received are summed over volumes; a peer whose link is down
// reports no counters and gets none.
func TestParseNodeStatesTrafficCounters(t *testing.T) {
	out := `[{"name":"r","role":"Primary",
	  "devices":[{"volume":0,"disk-state":"UpToDate","read":100,"written":1000},
	             {"volume":1,"disk-state":"UpToDate","read":20,"written":300}],
	  "connections":[
	    {"name":"b","connection-state":"Connected","peer-role":"Secondary","peer_devices":[
	      {"volume":0,"replication-state":"Established","peer-disk-state":"UpToDate","sent":900,"received":4},
	      {"volume":1,"replication-state":"Established","peer-disk-state":"UpToDate","sent":250,"received":1}]},
	    {"name":"c","connection-state":"Connecting","peer_devices":[]}]}]`

	states, err := parseNodeStatesFromJSON(out, "a")
	require.NoError(t, err)

	a := states["a"]
	require.NotNil(t, a.WrittenKiB)
	require.NotNil(t, a.ReadKiB)
	assert.Equal(t, uint64(1300), *a.WrittenKiB)
	assert.Equal(t, uint64(120), *a.ReadKiB)
	assert.Nil(t, a.SentKiB)

	b := states["b"]
	require.NotNil(t, b.SentKiB)
	require.NotNil(t, b.ReceivedKiB)
	assert.Equal(t, uint64(1150), *b.SentKiB)
	assert.Equal(t, uint64(5), *b.ReceivedKiB)
	assert.Nil(t, b.WrittenKiB)

	assert.Nil(t, states["c"].SentKiB)
	assert.Nil(t, states["c"].ReceivedKiB)
}

// One volume without a counter makes the sum unknown rather than short.
func TestParseNodeStatesPartialCounterIsUnknown(t *testing.T) {
	out := `[{"name":"r","role":"Primary",
	  "devices":[{"volume":0,"written":1000},{"volume":1}],
	  "connections":[{"name":"b","connection-state":"Connected","peer_devices":[
	      {"volume":0,"sent":900},{"volume":1}]}]}]`

	states, err := parseNodeStatesFromJSON(out, "a")
	require.NoError(t, err)
	assert.Nil(t, states["a"].WrittenKiB)
	assert.Nil(t, states["b"].SentKiB)
}
