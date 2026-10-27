import random, sys
src = open(sys.argv[1]).read()
src = src[:src.index('print("DPI v2')]
sys.argv = [sys.argv[1], sys.argv[2]]
g = {"__name__": "x"}
exec(compile(src, "dpi_eval2", "exec"), g)
def fixed(n):
    def gen():
        return g["beh_feats"](n, abs(random.gauss(n * 0.1, 0.25)), random.uniform(0.30, 0.60), abs(random.gauss(7200, 3600)) + 600)
    return gen
for n in (3, 6):
    random.seed(7)
    one = g["evaluate"](lambda: [fixed(n)()[0]], lambda: [g["crowd_behavior"]()[0]])
    random.seed(7)
    allf = g["evaluate"](fixed(n), g["crowd_behavior"])
    print(n, "%.3f %.3f" % (one, allf))
