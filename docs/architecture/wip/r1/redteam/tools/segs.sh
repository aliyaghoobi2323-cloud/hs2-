#!/bin/bash
# the five C3 tests of the seven known red ones, run with HQGSegs set from
# RED_HQG_SEGS by a temporary file that is removed at once (never committed)
cd /root/hs2-wt/r1-red/hs2-src/engine
f=zz_red_segs_test.go
printf "package engine\n\nimport (\n\t\"os\"\n\t\"strconv\"\n)\n\nfunc init() {\n\tif v, err := strconv.Atoi(os.Getenv(\"RED_HQG_SEGS\")); err == nil {\n\t\tHQGSegs = v\n\t}\n}\n" > $f
trap "rm -f $f" EXIT
for s in 0 8; do
	echo "### RED_HQG_SEGS=$s"
	RED_HQG_SEGS=$s /usr/local/go/bin/go test -count=1 -run "^(TestHostWaitBound|TestLowatAdapts|TestLowatAdaptsLinux|TestHostQLinuxSocket|TestHostQStatusLine)\$" -v . 2>&1 | grep -E "^(--- |ok|FAIL|PASS)|hostq"
done
