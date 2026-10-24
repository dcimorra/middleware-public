package installer

import (
	"context"
	"time"

	"github.com/dcimorra/middleware-public/internal/nodeenv"
)

// nvm on every machine, with the current LTS. The house installs any Node
// a site asks for through nvm when it is first needed (nodeenv); doing the
// nvm part here means a site's first start, or a site moved in from
// another node, does not wait for a git clone, and a machine where nvm
// cannot be fetched says so at install, not in a site's log weeks later.

func prepareNode(root string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	return (&nodeenv.Manager{Root: root}).Prepare(ctx, "lts")
}

func (in *Installer) stepNode() (bool, string, error) {
	if in.Node == nil {
		return false, "skipped", nil
	}
	return in.Node(in.Params.Root)
}
