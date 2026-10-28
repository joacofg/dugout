---
description: Shows or changes Dugout's autonomy mode in this repo (auto, scoped or supervised)
argument-hint: "[auto|scoped|supervised]"
allowed-tools: Bash(sh:*)
disable-model-invocation: true
---

!`sh "${CLAUDE_PLUGIN_ROOT}/bin/dugout-run" mode "$ARGUMENTS"`

Above is what Dugout said about this repo's autonomy mode. Tell your human in one line. If the mode changed, follow its rules from now on for conflicts with other branches: Dugout's next warning repeats them to you.
