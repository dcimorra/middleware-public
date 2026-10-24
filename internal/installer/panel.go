package installer

import (
	"path/filepath"
	"strings"
)

// PanelFinished, PanelRestart and PanelFailed start the last line of an
// install the panel started: the panel reads the log, not the unit, which
// is gone once it exits. PanelRestart is finished, with the operator just
// added to docker's group: the supervisor sees it once restarted.
const (
	PanelFinished = "middleware requirements: finished"
	PanelRestart  = "middleware requirements: finished, restart the house"
	PanelFailed   = "middleware requirements: failed: "
)

// stepOwnership hands to the operator what an install run as root made
// under the root: the nvm clone, the browser.
// Left as root's, the supervisor (the operator's user) could not write
// them, and the first site that asks for a Node would fail on a folder of
// its own house. Walked shallow, but modules whole: a root of sites with
// their node_modules is millions of files, none of them the installer's.
func (in *Installer) stepOwnership() (bool, string, error) {
	if !in.AsRoot {
		return false, "not run as root", nil
	}
	owner := in.Params.User + ":" + in.Params.Group
	root := in.Params.Root
	var did []string
	for _, args := range [][]string{
		{root, "-xdev", "-maxdepth", "2", "-user", "0"},
		{filepath.Join(root, "modules"), "-xdev", "-user", "0"},
	} {
		out, err := in.Exec.Sudo("find", append(args, "-print", "-exec", "chown", "-h", owner, "{}", "+")...)
		if err != nil {
			return len(did) > 0, "", err
		}
		for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
			if l != "" {
				did = append(did, l)
			}
		}
	}
	if len(did) == 0 {
		return false, "everything under the root is " + in.Params.User + "'s", nil
	}
	return true, "handed to " + owner + ": " + strings.Join(did[:min(len(did), 5)], ", ") + map[bool]string{true: ", …", false: ""}[len(did) > 5], nil
}
