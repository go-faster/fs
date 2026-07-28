package transport

import (
	"net/http"
	"time"

	"github.com/go-faster/fs/internal/cluster"
)

// SignForTest stamps req with cluster auth headers the server accepts. It lets
// a test build a request Client would refuse to build, which is the only way to
// exercise what the server does with a peer that does not play by the rules.
func SignForTest(secret Secret, req *http.Request, node cluster.NodeID) {
	secret.authenticate(req, node, time.Now())
}
