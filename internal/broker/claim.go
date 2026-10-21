package broker

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"baton/internal/engine"
	"baton/internal/harbor"
)

// Engines also load outside baton: a model picked in Unsloth Studio's UI,
// TabbyAPI started from its own start.bat, a ComfyUI job queued without the
// gate node. Baton treats such a load as a claim: the newest load wins, and
// whatever shares a card with it is released, exactly as if baton had been
// asked for the switch. During a game or hold nothing may load at all.

// filetimeToTime converts a Windows FILETIME (100 ns ticks since 1601).
func filetimeToTime(ft int64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	const epochDiff = 116444736000000000
	return time.Unix(0, (ft-epochDiff)*100)
}

// occupies reports whether s holds or is loading onto any card.
func occupies(s engine.Status) bool { return s.Loaded || s.Loading }

// trackSinceLocked records when each engine started occupying the cards.
// On the first cycle the age comes from the engine's newest process, so a
// collision found at startup still has a newest party.
func (b *Broker) trackSinceLocked(statuses []engine.Status, first bool, now time.Time) {
	for _, s := range statuses {
		if !occupies(s) {
			delete(b.since, s.Name)
			continue
		}
		if !b.since[s.Name].IsZero() {
			continue
		}
		t := now
		if first {
			var newest int64
			for _, p := range s.Procs() {
				if p.Start > newest {
					newest = p.Start
				}
			}
			if nt := filetimeToTime(newest); !nt.IsZero() {
				t = nt
			}
		}
		b.since[s.Name] = t
	}
}

// cardsOf is where an engine actually holds memory, or its configured cards
// while it has not allocated yet (still loading).
func (b *Broker) cardsOf(e engine.Engine, s engine.Status) []int {
	var out []int
	for g, mib := range s.VRAMMiB {
		if mib > b.residual() {
			out = append(out, g)
		}
	}
	if len(out) == 0 {
		return e.Config().GPUs
	}
	sort.Ints(out)
	return out
}

// claimLocked looks for engines loaded outside baton and settles them. It
// runs with b.mu held and only when no op is running or queued.
func (b *Broker) claimLocked() {
	if held(b.mode) {
		for _, e := range b.engines {
			name := e.Config().Name
			if !occupies(b.st[name]) {
				continue
			}
			b.ev.Warnf("claim", name, "%s loaded while the GPUs are reserved (%s); releasing it", name, b.modeWhy)
			eng := e
			go b.submit("enforce:"+name, "release "+name+" ("+string(b.mode)+")", false, func(ctx context.Context) error {
				if err := b.drain(ctx, eng, b.cfg.Policy.Game.Drain.Duration, true); err != nil {
					return err
				}
				return b.settled(b.releaseOne(ctx, eng, eng.Config().GameRelease, b.allGPUs()))
			})
			return
		}
		return
	}

	// Newest occupant first.
	type occ struct {
		e     engine.Engine
		s     engine.Status
		since time.Time
		cards []int
	}
	var occs []occ
	for _, e := range b.engines {
		s := b.st[e.Config().Name]
		if occupies(s) {
			occs = append(occs, occ{e, s, b.since[s.Name], b.cardsOf(e, s)})
		}
	}
	if len(occs) < 2 && !(b.mode == Stream && len(occs) == 1) {
		return
	}
	sort.SliceStable(occs, func(i, j int) bool { return occs[i].since.After(occs[j].since) })

	// While streaming, ComfyUI may not hold the cards at all.
	if b.mode == Stream && b.cfg.Policy.Stream.HoldComfy {
		for _, o := range occs {
			if o.e.Config().Kind == "comfyui" && o.s.Loaded {
				name := o.s.Name
				b.ev.Infof("claim", name, "ComfyUI loaded models during the stream; freeing it")
				eng := o.e
				go b.submit("enforce:"+name, "free "+name+" (stream)", false, func(ctx context.Context) error {
					if err := b.drain(ctx, eng, b.cfg.Policy.Stream.ComfyDrain.Duration, true); err != nil {
						return err
					}
					return b.settled(b.releaseOne(ctx, eng, "free", eng.Config().GPUs))
				})
				return
			}
		}
	}
	if len(occs) < 2 {
		return
	}

	winner := occs[0]
	var victims []engine.Engine
	var names []string
	for _, o := range occs[1:] {
		if overlap(o.cards, winner.cards) {
			victims = append(victims, o.e)
			names = append(names, o.s.Name)
		}
	}
	if len(victims) == 0 {
		return // on different cards: they fit side by side
	}
	if !winner.since.After(occs[1].since) {
		// Same age (both found at startup with no process times): keep the
		// holder, warn once.
		if b.noticeLocked("collision:"+winner.s.Name, 10*time.Minute) {
			b.ev.Warnf("collision", "", "%s and %s are both loaded on the same GPU(s)", winner.s.Name, strings.Join(names, ", "))
		}
		return
	}
	wname := winner.s.Name
	what := "loaded"
	if winner.s.Loading && !winner.s.Loaded {
		what = "started loading"
	}
	b.ev.Infof("claim", wname, "%s %s outside baton; releasing %s", wname, what, strings.Join(names, ", "))
	w := winner.e
	go b.submit("claim:"+wname, wname+" claimed the GPUs", false, func(ctx context.Context) error {
		for _, v := range victims {
			if err := b.drain(ctx, v, b.cfg.Policy.SwitchDrain.Duration, false); err != nil {
				return err
			}
			gpus := b.allGPUs()
			if err := b.releaseOne(ctx, v, v.Config().Release, gpus); err != nil {
				// The winner has loaded already: an aground victim is
				// reported, not a reason to stop.
				var ag *AgroundError
				if !errors.As(err, &ag) {
					return err
				}
			}
			if w.Config().Kind == "comfyui" && v.IsLLM() {
				b.mu.Lock()
				b.comfyTook = v.Config().Name
				b.mu.Unlock()
			}
		}
		b.mu.Lock()
		b.holder = wname
		b.mu.Unlock()
		return b.settled(nil)
	})
}

// settled counts a load outside baton that was settled without error.
func (b *Broker) settled(err error) error {
	if err == nil {
		b.count(func(c *harbor.Counters) { c.Claims++ })
	}
	return err
}
