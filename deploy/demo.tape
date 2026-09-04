# Records the kill -9 crash-recovery demo to docs/crash-demo.gif.
#
#   brew install vhs
#   docker compose -f deploy/docker-compose.yml up -d postgres && make serve   # API in another shell
#   vhs deploy/demo.tape
#
# Needs Ollama on the host with the model pulled (`make model-pull`).
# Run from the repo root.

Output docs/crash-demo.gif

Set Shell bash
Set FontSize 15
Set Width 1200
Set Height 820
Set Padding 16
Set Theme "Catppuccin Mocha"
Set TypingSpeed 40ms

Type "make crash-demo"
Sleep 800ms
Enter

# Wait for the prompt to come back once the script exits. (Wait+Screen does
# not see the bottom rows of a full screen, so match the cursor line instead.)
Wait+Line@180s /^>\s*$/
Sleep 6s
