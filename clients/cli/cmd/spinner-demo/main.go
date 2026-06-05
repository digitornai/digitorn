// Command spinner-demo animates every candidate spinner motif side by side in
// the terminal, at the CLI's real cadence (~240ms/frame), so a motif can be
// chosen by eye rather than from a static frame list. Ctrl+C to quit.
package main

import (
	"fmt"
	"os"
	"os/signal"
	"time"
)

type motif struct {
	name   string
	frames []string
}

var motifs = []motif{
	{"arc rotatif (actuel)", []string{"◜", "◠", "◝", "◞", "◡", "◟"}},
	{"étoile (= Claude, à éviter)", []string{"·", "✢", "✳", "✶", "✻", "✷", "✦"}},
	{"diamant pulsant (validé)", []string{"·", "◦", "◇", "◈", "◆", "◈", "◇", "◦"}},
	{"— diamant, plus de positions —", []string{" "}},
	{"pulse + tumble (long)", []string{"·", "◦", "◇", "◈", "◆", "◼", "◆", "◈", "◇", "◦"}},
	{"balayage (remplissage tourne)", []string{"◇", "◈", "◆", "◧", "◼", "◨", "◆", "◈"}},
	{"journey complet", []string{"·", "◇", "◆", "◧", "◼", "◨", "◆", "◇"}},
	{"facettes (triangles)", []string{"◇", "◆", "◤", "◥", "◢", "◣"}},
	{"4 orientations + respire", []string{"·", "◇", "◆", "◼", "◆", "◇", "◦", "◇", "◆", "◼"}},
}

const (
	tick = 120 * time.Millisecond // base animation tick
	slow = 2                      // hold each frame for `slow` ticks → 240ms/frame
	cyan = "\x1b[1;36m"
	dim  = "\x1b[2m"
	rst  = "\x1b[0m"
)

func main() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	fmt.Print("\x1b[?25l")       // hide cursor
	defer fmt.Print("\x1b[?25h") // restore on exit

	t := time.NewTicker(tick)
	defer t.Stop()
	frame := 0

	render := func() {
		var b []byte
		b = append(b, "\x1b[H\x1b[2J"...) // home + clear
		b = append(b, (dim + "  spinners — cadence réelle 240ms/frame · Ctrl+C pour quitter\n\n" + rst)...)
		for _, m := range motifs {
			g := m.frames[(frame/slow)%len(m.frames)]
			// glyph in a "loading…" line, like the real session/picker spinner
			line := fmt.Sprintf("   %s%s%s  loading…   %s%-22s%s\n", cyan, g, rst, dim, m.name, rst)
			b = append(b, line...)
		}
		os.Stdout.Write(b)
	}

	render()
	for {
		select {
		case <-t.C:
			frame++
			render()
		case <-sig:
			fmt.Print("\x1b[?25h\n")
			return
		}
	}
}
