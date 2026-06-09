package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

func main() {
	f, err := os.Open(`C:\Users\ASUS\.gemini\antigravity\brain\ad3757d7-83a2-493d-ad5c-6bc8fb3c901e\.system_generated\logs\transcript.jsonl`)
	if err != nil { panic(err) }
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var line map[string]any
		json.Unmarshal(scanner.Bytes(), &line)
		if line["type"] == "PLANNER_RESPONSE" {
			tc, ok := line["tool_calls"].([]any)
			if !ok { continue }
			for _, callAny := range tc {
				call := callAny.(map[string]any)
				if call["name"] == "filesystem.write" || call["name"] == "write" {
					argsAny := call["args"]
					if argsAny == nil { continue }
					args := argsAny.(map[string]any)
					if path, ok := args["path"].(string); ok && strings.Contains(path, "globals.css") {
						b, _ := json.MarshalIndent(args, "", "  ")
						fmt.Println(string(b))
					}
				}
			}
		}
	}
}
