package main

import (
	"fmt"

	haifypb "github.com/haify-project/haify/api/proto/v1"
)

// replicationEncryption describes the resource's replication links from the
// node states: every peer entry carries a replication state, and tls when its
// connection runs over kernel TLS. The answering node's own entry has none.
func replicationEncryption(states map[string]*haifypb.NodeResourceState) string {
	links, tls := 0, 0
	for _, st := range states {
		if st.GetReplicationState() == "" && !st.GetTls() {
			continue
		}
		links++
		if st.GetTls() {
			tls++
		}
	}
	switch {
	case links > 0 && tls == links:
		return "Replication between nodes is encrypted (kernel TLS on every connection)."
	case tls > 0:
		return fmt.Sprintf("Replication between nodes is encrypted on %d of %d connections only.", tls, links)
	default:
		return "Replication between nodes is NOT encrypted."
	}
}
