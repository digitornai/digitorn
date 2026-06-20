package main

import (
	"fmt"
	"github.com/mbathepaul/digitorn/internal/runtime/context/repomap"
	_ "github.com/mbathepaul/digitorn/internal/modules/filesystem"
)

func main() {
	root := "/home/paul/codes/digitorn"
	out := repomap.Get(root)
	fmt.Println(out)
}
