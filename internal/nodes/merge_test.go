package nodes

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/dcimorra/the-middleware/internal/wireguard"
)

func pubKey(t *testing.T) wireguard.Key {
	k, err := wireguard.GeneratePrivateKey()
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := k.Public()
	return pub
}

// rosterOf is a node that already knows a and x, x admitted by a.
func rosterOf(t *testing.T) (*Mesh, Member, Member) {
	m := &Mesh{now: time.Now, seen: map[string]time.Time{}}
	m.self = Self{ID: "self"}
	m.st.IP = netip.MustParseAddr("10.66.0.1")
	t0 := time.Now().Add(-time.Hour)
	a := Member{ID: "a", Name: "a", IP: netip.MustParseAddr("10.66.0.2"), PublicKey: pubKey(t), Updated: t0}
	x := Member{ID: "x", Name: "x", IP: netip.MustParseAddr("10.66.0.3"), PublicKey: pubKey(t), Updated: t0, Joined: t0, By: "a"}
	m.st.Members = []Member{a, x}
	return m, a, x
}

func (m *Mesh) entry(id string) Member {
	for _, x := range m.st.Members {
		if x.ID == id {
			return x
		}
	}
	return Member{}
}

// A node that dates its own entry in the year 2100 gets a minute ahead
// of this clock, and an expel issued now still ends it.
func TestAnEntryFromTheFutureDoesNotOutliveAnExpel(t *testing.T) {
	m, _, x := rosterOf(t)
	x.Updated = time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
	x.Name = "x-2100"
	m.mergeLocked("x", []Member{x})
	got := m.entry("x")
	if got.Name != "x-2100" || got.Updated.After(time.Now().Add(MaxSkew)) {
		t.Fatalf("the date was not clamped: %v", got.Updated)
	}
	// a expels it now; x keeps announcing itself from the future.
	removal := got
	removal.Removed, removal.Updated = true, time.Now()
	m.mergeLocked("a", []Member{removal})
	m.mergeLocked("x", []Member{x})
	if !m.entry("x").Removed {
		t.Fatal("the node's own date in the future outran its expel")
	}
	// A removal of this node dated ahead is still seen.
	m.st.Updated = time.Now()
	if _, ex := m.mergeLocked("a", []Member{{ID: "self", IP: m.st.IP, PublicKey: pubKey(t), Removed: true, Updated: time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)}}); !ex {
		t.Fatal("a removal of this node was missed")
	}
}

// Another node's word about x does not change x: only x does, and
// whoever relays it cannot give x its own key.
func TestOnlyANodeChangesItsOwnEntry(t *testing.T) {
	m, _, x := rosterOf(t)
	forged := x
	forged.PublicKey, forged.Name, forged.Updated = pubKey(t), "forged", time.Now()
	if changed, _ := m.mergeLocked("a", []Member{forged}); changed || m.entry("x").PublicKey != x.PublicKey {
		t.Fatal("a third node rewrote x's key")
	}
	if changed, _ := m.mergeLocked("x", []Member{forged}); !changed || m.entry("x").Name != "forged" {
		t.Fatal("x's own change was not taken")
	}
	if e := m.entry("x"); e.By != "a" || !e.Joined.Equal(x.Joined) {
		t.Fatalf("x's word about itself rewrote its admission: %+v", e)
	}
	// Dated later and saying the same is not a change.
	again := m.entry("x")
	again.Updated = time.Now().Add(time.Second)
	if changed, _ := m.mergeLocked("x", []Member{again}); changed {
		t.Fatal("the same entry re-dated counted as a change")
	}
	// A node it did not know is taken from whoever names it.
	n := Member{ID: "n", Name: "n", IP: netip.MustParseAddr("10.66.0.9"), PublicKey: pubKey(t), Updated: time.Now(), Joined: time.Now(), By: "a"}
	if changed, _ := m.mergeLocked("x", []Member{n}); !changed || len(m.liveLocked()) != 3 {
		t.Fatal("a node introduced by another was not learnt")
	}
}

// A removal is final: neither a newer live entry brings the node back,
// nor an admission it wrote for itself, nor one read from a node that
// did not admit it, nor one at its old address. A join through the node
// that admitted it, after the removal and at a new address, does.
func TestARemovedNodeComesBackOnlyThroughAJoin(t *testing.T) {
	m, _, x := rosterOf(t)
	removal := x
	removal.Removed, removal.Updated = true, time.Now().Add(-time.Minute)
	m.mergeLocked("a", []Member{removal})
	live := x
	live.Updated = time.Now()
	self := live
	self.IP, self.Joined, self.By = netip.MustParseAddr("10.66.0.40"), time.Now(), "x"
	relayed := live
	relayed.IP, relayed.Joined, relayed.By = netip.MustParseAddr("10.66.0.41"), time.Now(), "b"
	sameIP := live
	sameIP.Joined, sameIP.By = time.Now(), "a"
	for _, in := range []struct {
		name, from string
		m          Member
	}{{"newer", "x", live}, {"self-admitted", "x", self}, {"relayed", "a", relayed}, {"same address", "a", sameIP}} {
		m.mergeLocked(in.from, []Member{in.m})
		if !m.entry("x").Removed {
			t.Fatalf("%s: the removed node came back", in.name)
		}
	}
	join := live
	join.IP, join.PublicKey, join.Joined, join.By = netip.MustParseAddr("10.66.0.42"), pubKey(t), time.Now(), "a"
	if m.mergeLocked("a", []Member{join}); m.entry("x").Removed || m.entry("x").IP != join.IP {
		t.Fatalf("a new join did not bring it back: %+v", m.entry("x"))
	}
	// And an old copy of the removal does not end the new admission.
	if m.mergeLocked("b", []Member{removal}); m.entry("x").Removed {
		t.Fatal("the removal before the join ended it")
	}
}

// An address outside the mesh, or one another node already has, never
// reaches WireGuard's AllowedIPs.
func TestAnEntryWithAnAddressThatIsNotItsOwnIsDropped(t *testing.T) {
	m, a, x := rosterOf(t)
	for name, ip := range map[string]string{"public": "203.0.113.7", "a's": a.IP.String(), "this node's": m.st.IP.String()} {
		n := Member{ID: "n-" + name, Name: name, IP: netip.MustParseAddr(ip), PublicKey: pubKey(t), Updated: time.Now()}
		if changed, _ := m.mergeLocked("a", []Member{n}); changed {
			t.Fatalf("a node at %s address was taken in", name)
		}
	}
	moved := x
	moved.IP, moved.Updated = a.IP, time.Now()
	if m.mergeLocked("x", []Member{moved}); m.entry("x").IP != x.IP {
		t.Fatal("x took a's address")
	}
}

// Through the doors: alpha expels gamma, and gamma comes back only by
// joining again, through beta, which alpha then learns in a round.
func TestAnExpelledNodeRejoinsThroughAnInvitation(t *testing.T) {
	ctx := context.Background()
	port := freePort(t)
	a, b, c := newNode(t, "alpha", port), newNode(t, "beta", port), newNode(t, "gamma", port)
	code, _, err := a.Invite(ctx, "198.51.100.1:51821")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Join(ctx, code, ""); err != nil {
		t.Fatal(err)
	}
	code, _, _ = a.Invite(ctx, "")
	if _, err := c.Join(ctx, code, ""); err != nil {
		t.Fatal(err)
	}
	b.Gossip(ctx)
	if _, err := a.Expel(ctx, c.Self().ID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	b.Gossip(ctx)
	if len(b.Members()) != 1 {
		t.Fatalf("beta still has gamma: %v", ids(b.Members()))
	}
	code, _, _ = b.Invite(ctx, "")
	if _, err := c.Join(ctx, code, ""); err != nil {
		t.Fatal(err)
	}
	a.Gossip(ctx)
	if !ids(a.Members())["gamma"] || len(a.net.peers()) != 2 {
		t.Fatalf("alpha did not learn gamma's new join: %v", ids(a.Members()))
	}
}
