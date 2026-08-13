# Regenerate from the fleet directory:
#   cd <fleet> && vhs <pilot>/docs/readme/status.tape
Output vhs-scratch-status.gif

Set Shell "bash"
Set FontSize 15
Set FontFamily "Maple Mono NF"
Set Width 1000
Set Height 385
Set Padding 20
Set WindowBar Colorful

Hide
Type "clear"
Enter
Show
Type "pilot status"
Enter
Sleep 8s
Screenshot shots/status.png
