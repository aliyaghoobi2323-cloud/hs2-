package engine

import "os"

// b463 HS2_B463_NORAW=1 (Iran only, default off; diagnostic, not for prod):
// the lite opener never marks a first sub raw, so every lite conn runs on
// records from byte 0 like a hedged conn (HEDGE2/3/4) but with no hedge sub.
// It tests the lead for HEDGE4's burst p50 cost (+4..+9 ms in 3 WAN blocks):
// a hedged conn's first sub is records, not raw, for the whole burst. If
// records alone cost the same, a raw-kept hedge (lab/b455/PROGRESS.txt
// b463 design) is worth building; if not, the cost is elsewhere.
//
// Mixed-version safe: the exit gets role 1 (mconnRoleFirst), which every
// release since mconn lite reads.
var noRaw463 = os.Getenv("HS2_B463_NORAW") == "1"
