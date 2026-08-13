# Regenerate from the fleet directory:
#   cd <fleet> && vhs <pilot>/docs/readme/doctor.tape
# The viewport is sized to the edge section deliberately — full doctor
# output includes host findings that have no business in a README.
Output vhs-scratch-doctor.gif

Set Shell "bash"
Set FontSize 15
Set FontFamily "Maple Mono NF"
Set Width 1000
Set Height 360
Set Padding 20
Set WindowBar Colorful

Hide
Type "clear"
Enter
Show
Type "pilot doctor"
Enter
Sleep 25s
Screenshot shots/doctor.png
