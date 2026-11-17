#!/bin/bash
# q84.sh SLOTSEQ: run a sequence of arms in one slot (labslot.sh lb84 runs back to back)
# usage: labslot.sh bash q84.sh A1 B1 A2 B2 ... (A* = keys off, B* = ARMB keys)
for t in "$@"; do
  case $t in A*|O*) ARM="";; B*) ARM="${ARMB}";; C*) ARM="${ARMC:-}";; esac
  ARM="$ARM" bash /opt/hs2lab/lb84/lb84.sh $t
done
