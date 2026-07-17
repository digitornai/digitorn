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
	tick = 120 * time.Millisecond
	slow = 2
	cyan = "\x1b[1;36m"
	dim  = "\x1b[2m"
	rst  = "\x1b[0m"
)

func main() {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)

	fmt.Print("\x1b[?25l")
	defer fmt.Print("\x1b[?25h")

	t := time.NewTicker(tick)
	defer t.Stop()
	frame := 0

	render := func() {
		var b []byte
		b = append(b, "\x1b[H\x1b[2J"...)
		b = append(b, (dim + "  spinners — cadence réelle 240ms/frame · Ctrl+C pour quitter\n\n" + rst)...)
		for _, m := range motifs {
			g := m.frames[(frame/slow)%len(m.frames)]
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
