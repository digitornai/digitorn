package index

import "fmt"

func fatal(v ...any)                 { panic(fmt.Sprint(v...)) }
func fatalf(format string, v ...any) { panic(fmt.Sprintf(format, v...)) }
