# The README's demo: two people's Claude Code sessions on one repo, a
# conflict Dugout catches before the merge, and the office view. Hook events
# and MCP calls are played as Claude Code sends them (docs/demo.sh), so it
# needs no model and costs nothing. From the repo root, with vhs, ffmpeg, go,
# git, jq and Google Chrome:
#
#   vhs docs/demo.tape
#   bash -c 'source docs/demo.sh && demo_compose /tmp/dugout-demo-terminal.mp4 /tmp/dugout-demo-office.png docs/demo.gif'

Output "/tmp/dugout-demo-terminal.mp4"

Require go
Require git
Require jq

Set Shell bash
Set FontSize 16
Set Width 1200
Set Height 720
Set Padding 28
Set TypingSpeed 18ms
Set Theme "Builtin Light"

Hide
Type "export PS1='$ ' && source docs/demo.sh && demo_start && clear" Enter
Wait@180s /^\$$/
Type "# alice and bob each run Claude Code on their own branch of acme/app." Enter
Show

Sleep 1.5s
Type "intent alice 'Show a clearer error when the login fails'" Enter
Sleep 700ms
Type "intent bob 'Rate-limit failed login attempts'" Enter
Sleep 1s
Type `edit alice api.go 9 'http.Error(w, "that user and password do not match", 401)'` Enter
Sleep 1.2s
Type `edit bob api.go 9 'limiter.Fail(user); http.Error(w, "wrong user or password", 401)'` Enter
Sleep 1s
Type "# bob sends the next prompt, and Dugout tells them in one line:" Enter
Type "prompt bob" Enter
Wait+Screen@20s /conflicts with branch/
Sleep 2s
Type "# bob's agent tries to edit api.go again:" Enter
Type `edit bob api.go 9 'limiter.Fail(user)'` Enter
Wait+Screen@10s /<.dugout-context>/
Sleep 5s
Hide
Type "office_shot /tmp/dugout-demo-office.png && clear" Enter
Sleep 8s
Show
Type "dugout status" Enter
Wait+Screen@10s /overlap/
Sleep 4s
Type "dugout office -no-open    # the live office view, read-only" Enter
Sleep 1.5s
Hide
Type "demo_stop" Enter
Sleep 1s
