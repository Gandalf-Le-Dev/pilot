# Regenerate from the repo root:  vhs docs/readme/doctor.tape
# `pilot` is aliased to the demo renderer — real output code, example data.
Output vhs-scratch-doctor.gif

Set Shell "bash"
Set FontSize 15
Set FontFamily "Maple Mono NF"
Set Width 1000
Set Height 510
Set Padding 20
Set WindowBar Colorful

Hide
Type "alias pilot='go run ./docs/readme/demo' && go run ./docs/readme/demo status >/dev/null 2>&1 && clear"
Enter
Show
Type "pilot doctor"
Enter
Sleep 6s
Screenshot shots/doctor.png
