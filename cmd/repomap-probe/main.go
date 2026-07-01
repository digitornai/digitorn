package main

import (
	"fmt"
	"github.com/digitornai/digitorn/internal/runtime/context/repomap"
	_ "github.com/digitornai/digitorn/internal/modules/filesystem"
)

func main() {
	root := "/home/paul/codes/digitorn"
	out := repomap.Get(root)
	fmt.Println(out)
}
