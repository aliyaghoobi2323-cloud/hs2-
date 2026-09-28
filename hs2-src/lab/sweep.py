#!/usr/bin/env python3
"""Run many lab experiments (lab/run.sh) and collect one JSON line each.

    lab/sweep.py plan.json results.jsonl [--jobs 2]

plan.json is a list of objects; each is a set of environment variables for
run.sh (MODE, RATE, HS2_TUNE_*, IRAN_EXTRA, ...) plus optional "label" and
"rep". Finished runs already in results.jsonl are skipped, so a sweep can be
resumed. Keep --jobs at or below half the CPU count: the emulator needs a core
to keep its timing honest.
"""
import json, os, subprocess, sys, threading, queue, time

def key(run):
    return json.dumps({k: v for k, v in sorted(run.items())}, sort_keys=True)

def main():
    plan_path, out_path = sys.argv[1], sys.argv[2]
    jobs = int(sys.argv[sys.argv.index('--jobs') + 1]) if '--jobs' in sys.argv else 2
    here = os.path.dirname(os.path.abspath(__file__))
    plan = json.load(open(plan_path))
    done = set()
    if os.path.exists(out_path):
        for line in open(out_path):
            try:
                done.add(key(json.loads(line)['run']))
            except Exception:
                pass
    todo = [r for r in plan if key(r) not in done]
    print(f"{len(plan)} runs planned, {len(todo)} to go, {jobs} in parallel", flush=True)
    q = queue.Queue()
    for r in todo:
        q.put(r)
    lock = threading.Lock()
    t0 = time.time()
    count = [0]

    def worker(wid):
        while True:
            try:
                run = q.get_nowait()
            except queue.Empty:
                return
            env = dict(os.environ)
            for k, v in run.items():
                if k not in ('label', 'rep'):
                    env[k] = str(v)
            env['LAB_ID'] = str(wid)
            p = subprocess.run([os.path.join(here, 'run.sh')], env=env,
                               capture_output=True, text=True, timeout=300)
            line = p.stdout.strip().splitlines()[-1] if p.stdout.strip() else ''
            try:
                res = json.loads(line)
            except Exception:
                res = {'error': (p.stdout + p.stderr)[-500:]}
            with lock:
                count[0] += 1
                with open(out_path, 'a') as f:
                    f.write(json.dumps({'run': run, 'result': res}) + '\n')
                el = time.time() - t0
                print(f"[{count[0]}/{len(todo)} {el/60:.1f}min] {run.get('label','')} -> "
                      f"{(res.get('probe') or {}).get('mbps', 'ERR')}", flush=True)

    ts = [threading.Thread(target=worker, args=(i + 1,)) for i in range(jobs)]
    for t in ts:
        t.start()
    for t in ts:
        t.join()

if __name__ == '__main__':
    main()
