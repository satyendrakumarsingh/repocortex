package cli

import (
	"context"
	"net/http"

	"github.com/satyendrakumarsingh/repocortex/internal/config"
	"github.com/satyendrakumarsingh/repocortex/internal/portal"
	"github.com/satyendrakumarsingh/repocortex/internal/restapi"
)

// newNextPortal assembles the redesigned portal: static assets plus the tool
// endpoint, over the read-only index.
//
// It uses the `full` profile whatever .repocortex.yml says. A profile exists to
// keep tool schemas out of a model's context; a web page pays nothing for a
// tool it does not call, and the Health section needs the full-profile tools.
func newNextPortal(ctx context.Context, g *Global) (http.Handler, func(), error) {
	srv, closeServer, err := openAgentServer(ctx, g, agentServerOptions{Profile: config.MCPFull})
	if err != nil {
		return nil, nil, err
	}
	inv, err := srv.NewInvoker(ctx, "portal")
	if err != nil {
		closeServer()
		return nil, nil, err
	}
	api := restapi.Handler(inv, restapi.Info{
		Project: g.Config().ProjectName,
		Version: Version,
		Profile: string(config.MCPFull),
	})
	return portal.Handler(api), func() {
		_ = inv.Close()
		closeServer()
	}, nil
}
