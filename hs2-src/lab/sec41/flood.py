import socket, sys, time
n = int(sys.argv[1]); a = (sys.argv[2], int(sys.argv[3]))
ss = []
for i in range(n):
    try:
        s = socket.create_connection(a, timeout=3); ss.append(s)
    except Exception as e:
        pass
print("open", len(ss), flush=True)
time.sleep(6)
closed = 0
for s in ss:
    s.setblocking(False)
    try:
        if s.recv(1) == b"": closed += 1
    except BlockingIOError: pass
    except Exception: closed += 1
print("closed_by_server_within_6s", closed)
