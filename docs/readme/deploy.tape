# Regenerate from the fleet directory (expects a service with something
# to activate — a preceding rollback makes the deploy real):
#   cd <fleet> && vhs <pilot>/docs/readme/deploy.tape
Output vhs-scratch-deploy.gif

Set Shell "bash"
Set FontSize 15
Set FontFamily "Maple Mono NF"
Set Width 1000
Set Height 580
Set Padding 20
Set WindowBar Colorful

Hide
Type "clear"
Enter
Show
Type "pilot deploy hopbox-docs"
Enter
Sleep 150s
Screenshot shots/deploy.png
