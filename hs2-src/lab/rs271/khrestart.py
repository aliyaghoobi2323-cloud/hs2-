import os, sys
# khrestart.py ENVIRON CMDLINE CWD : exec the saved kharej hs2 command line with its saved environment
env = dict(x.split("=", 1) for x in open(sys.argv[1], "rb").read().decode().split("\0") if "=" in x)
a = [x for x in open(sys.argv[2], "rb").read().decode().split("\0") if x]
os.chdir(sys.argv[3])
os.execve(a[0], a, env)
