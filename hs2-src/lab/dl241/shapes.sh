for l in $(awk "\$4==\"dir=d\"{print \$1}" $1/tests.txt); do python3 -c "
import json,sys;d=json.load(open(sys.argv[1]));print(sys.argv[2],[round(i[\"sum\"][\"bits_per_second\"]/1e6) for i in d[\"intervals\"]])" $1/tests/t$l.json t$l 2>/dev/null; done
