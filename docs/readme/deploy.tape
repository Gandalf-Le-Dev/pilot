# Regenerate from the repo root:  vhs docs/readme/deploy.tape
# `pilot` is aliased to the demo renderer — real output code, example data.
Output vhs-scratch-deploy.gif

Set Shell "bash"
Set FontSize 15
Set FontFamily "Maple Mono NF"
Set Width 1000
Set Height 580
Set Padding 20
Set WindowBar Colorful

Hide
Type "alias pilot='go run ./docs/readme/demo' && go run ./docs/readme/demo status >/dev/null 2>&1 && clear"
Enter
Show
Type "pilot deploy my-app"
Enter
Sleep 6s
Screenshot shots/deploy.png
