#!/bin/bash
# fresh containers + fixtures (panel echo, late 2nd IP on kharej, certs)
for n in ir:10.30.0.10:iran kh:10.30.0.20:kharej; do IFS=: read name ip host <<<"$n"
  docker rm -f $name >/dev/null 2>&1
  docker run -d --name $name --hostname $host --privileged --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw \
    --tmpfs /run --tmpfs /run/lock --network hs2net --ip $ip --restart unless-stopped hs2sysd >/dev/null
done
sleep 8
docker exec kh bash -c 'cat > /usr/local/bin/panel.py <<PY
import socket,threading
s=socket.socket();s.setsockopt(socket.SOL_SOCKET,socket.SO_REUSEADDR,1);s.bind(("127.0.0.1",8443));s.listen(64)
def h(c):
  try:
    while True:
      d=c.recv(65536)
      if not d:break
      c.sendall(d)
  except Exception:pass
  c.close()
while True:
  c,_=s.accept();threading.Thread(target=h,args=(c,),daemon=True).start()
PY
printf "[Unit]\nDescription=fake panel\n[Service]\nExecStart=/usr/bin/python3 /usr/local/bin/panel.py\nRestart=always\n[Install]\nWantedBy=multi-user.target\n" > /etc/systemd/system/panel.service
printf "[Unit]\nDescription=late second IP\n[Service]\nType=oneshot\nExecStart=/bin/sh -c \"sleep 5; ip addr add 10.30.0.21/24 dev eth0 || true\"\n[Install]\nWantedBy=multi-user.target\n" > /etc/systemd/system/late-ip.service
systemctl daemon-reload; systemctl enable --now panel.service >/dev/null 2>&1; systemctl enable late-ip.service >/dev/null 2>&1; systemctl start late-ip.service'
docker exec ir bash -c 'openssl req -x509 -newkey rsa:2048 -keyout /root/key.pem -out /root/cert.pem -days 30 -nodes -subj "/CN=germanytunnel.test" -addext "subjectAltName=DNS:germanytunnel.test" >/dev/null 2>&1'
echo "fresh: ir=$(docker exec ir systemctl is-system-running) kh=$(docker exec kh systemctl is-system-running) kh-ips=$(docker exec kh ip -4 -o addr show eth0 | awk '{print $4}' | tr '\n' ' ')"
