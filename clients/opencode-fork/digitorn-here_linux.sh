#!/bin/bash
# filepath: /home/paul/codes/digitorn/clients/opencode-fork/digitorn-here_linux.sh

# Launch the opencode-fork TUI (digitorn daemon) with the AGENT working in the
# directory you call this from — while Bun still loads opencode's bunfig/tsconfig
# (JSX = Solid) from the package dir.

# Usage (from any project folder):
#   ./digitorn-here_linux.sh -a chat-simple -u http://localhost:8000 -g https://gateway.digitorn.ai

# Parse script arguments
while getopts "a:u:g:" opt; do
  case $opt in
    a) App="$OPTARG" ;;
    u) Url="$OPTARG" ;;
    g) Gateway="$OPTARG" ;;
    *) echo "Usage: $0 -a <App_Name> -u <Daemon_URL> -g <Gateway_URL>" >&2; exit 1 ;;
  esac
done

# Set defaults if not provided
App=${App:-"digitorn-code"}
Url=${Url:-"http://localhost:8000"}
Gateway=${Gateway:-"http://localhost:8002"} # Default Gateway URL

# Get current project directory
proj=$(pwd)
pkg="/home/paul/codes/digitorn/clients/opencode-fork/packages/opencode"

# Set environment variables
export DIGITORN_CWD="$proj"
export DIGITORN_URL="$Url"
export DIGITORN_APP="$App"
export DIGITORN_GATEWAY="$Gateway" # Setting the Gateway URL

# Print info
echo -e "▶ project (agent workdir): $proj"
echo -e "▶ app: $App   daemon: $Url   gateway: $Gateway"

# Change directory to the package dir, run Bun, and return to the original directory
pushd "$pkg" > /dev/null
trap 'popd > /dev/null' EXIT
bun run dev