| run | N | MIX | ok | links up at start (wait s) | edge serving / exit links (end) | same after idle | link sockets; re-dialed; silent | down / up Mbit/s | host TCP pressures; max pages; samples past tcp_mem[1] | CPU edge / exit % (mean) | panic / fatal / race in logs |
|---|---|---|---|---|---|---|---|---|---|---|---|
| 0a819aa3-m-1 | 64 | s5 | True | 66 (6.83) | 64 / 68 | 64 / 68 | 68; 0; 0 | 76.3 / 0.0 | 0; 178214; 0 | 62 / 51 | 0 / 0 / 0 |
| 3c28078d-m-1 | 300 | s5 | True | 301 (29.77) | 300 / 304 | 300 / 304 | 304; 0; 0 | 81.8 / 0.0 | 0; 329921; 0 | 69 / 69 | 0 / 0 / 0 |
| a076048b-m-1 | 600 | s5 | True | 602 (59.42) | 600 / 604 | 600 / 604 | 604; 2; 0 | 88.2 / 0.0 | 0; 301375; 0 | 76 / 94 | 0 / 0 / 0 |

Process samples (PROC_TRACE, every 5 s): before the users (first sample, the tunnel up) / peak / end of the users' window / after POST_IDLE (20 s after the client ended); growth = the change over the users' last 20 s (to 3 s before their end), % (a leak grows there; a heap that reached its working size is flat).

| run | side | fds | goroutines | RSS MB | threads | growth fds / gor / RSS % |
|---|---|---|---|---|---|---|
| 0a819aa3-m-1 | edge | 201 / 209 / 81 / 80 | 1035 / 1372 / 988 / 985 | 33.2 / 75.2 / 75.2 / 77.3 | 12 / 13 / 13 / 13 | 0.0 / 0.0 / 24.3 |
| 0a819aa3-m-1 | exit | 171 / 205 / 77 / 76 | 938 / 1293 / 1107 / 906 | 35.6 / 64.2 / 64.2 / 64.4 | 12 / 13 / 13 / 13 | 0.0 / 0.0 / 0.5 |
| 3c28078d-m-1 | edge | 372 / 717 / 317 / 316 | 4247 / 5492 / 4292 / 4289 | 83.3 / 168.7 / 168.7 / 179.9 | 12 / 14 / 14 / 14 | 0.0 / 0.0 / 16.7 |
| 3c28078d-m-1 | exit | 318 / 713 / 313 / 312 | 3938 / 5384 / 4539 / 3974 | 85.2 / 199.0 / 199.0 / 199.1 | 12 / 13 / 13 / 13 | 0.0 / 0.0 / 6.3 |
| a076048b-m-1 | edge | 786 / 1116 / 617 / 616 | 8461 / 9991 / 8492 / 8489 | 154.7 / 290.1 / 290.1 / 291.4 | 13 / 14 / 14 / 14 | 0.0 / 0.0 / 30.5 |
| a076048b-m-1 | exit | 739 / 1112 / 613 / 612 | 8022 / 9632 / 8317 / 7874 | 162.3 / 342.8 / 342.8 / 342.8 | 12 / 13 / 13 / 13 | 0.0 / -0.3 / 5.8 |
