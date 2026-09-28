#!/usr/bin/env python3
"""Be the DPI. Measure whether the shaped tunnel is separable from bulk HTTPS.

We generate flows as (packet_sizes, inter_arrival_gaps, first_byte_entropy) for
three sources: real bulk HTTPS, the UNSHAPED tunnel, and the SHAPED tunnel
(using the same length distribution and jittered pacing the Go shaper uses).
A logistic classifier trains on sizes+timing+entropy; AUC ~0.5 means the DPI
cannot separate the shaped tunnel from the crowd.
"""
import math, random, statistics as st
random.seed(2024)
K = 16

# ---- length distributions ----
HTTPS_SIZES = [(1400,.55),(1200,.08),(900,.05),(600,.05),(400,.05),(250,.06),(150,.06),(80,.06),(40,.04)]
def sample_dist(dist):
    u=random.random(); acc=0
    for s,w in dist:
        acc+=w
        if u<=acc/sum(x[1] for x in dist): return s
    return dist[-1][0]

def https_flow():
    sizes=[sample_dist(HTTPS_SIZES) for _ in range(K)]
    gaps=[abs(random.gauss(4,3)) for _ in range(K)]  # ms, bursty
    ent=random.gauss(5.0,0.3)  # TLS records: header structure lowers it a bit
    return sizes,gaps,ent

def unshaped_flow():
    # payload+30 correlation, fixed 5s keepalive beat, full entropy
    sizes=[114,50]+[random.choice([30,50,1410]) for _ in range(K-2)]
    gaps=[0,30]+[5000 if sizes[2+i]==30 else random.choice([1,2,20]) for i in range(K-2)]
    ent=random.gauss(7.9,0.05)
    return sizes,gaps,ent

def shaped_flow(entropy_fixed=False):
    # sizes from the SAME HTTPS distribution the Go sampler uses
    sizes=[sample_dist(HTTPS_SIZES) for _ in range(K)]
    # jittered token-bucket pacing: average gap ~4ms, +-40% jitter, micro-bursts
    gaps=[]
    for _ in range(K):
        base=random.gauss(4,1.5)
        gaps.append(abs(base*(1+(random.random()*2-1)*0.4)))
    # entropy: on UDP with the QUIC-ish prefix, first-byte entropy drops toward
    # HTTPS. entropy_fixed models "riding inside real TLS" (TCP/Reality path).
    ent = random.gauss(5.0,0.3) if entropy_fixed else random.gauss(7.5,0.2)
    return sizes,gaps,ent

def feats(s,g,e):
    f=s[:6]+[st.mean(s),st.pstdev(s),st.mean(g),st.pstdev(g),
             sum(1 for x in g if 3000<=x<=7000),e,len(set(s))]
    return f

def train(pos,neg,it=3000,lr=0.02):
    dim=len(pos[0]); allx=pos+neg
    mean=[st.mean(c) for c in zip(*allx)]; sd=[st.pstdev(c) or 1 for c in zip(*allx)]
    nrm=lambda x:[(a-m)/s for a,m,s in zip(x,mean,sd)]
    X=[(nrm(x),1) for x in pos]+[(nrm(x),0) for x in neg]
    w=[0.0]*dim; b=0.0
    for _ in range(it):
        random.shuffle(X)
        for x,y in X:
            z=b+sum(wi*xi for wi,xi in zip(w,x)); p=1/(1+math.exp(-max(-30,min(30,z)))); gr=p-y
            for j in range(dim): w[j]-=lr*gr*x[j]
            b-=lr*gr
    return w,b,mean,sd
def score(m,x):
    w,b,mean,sd=m; xn=[(a-mm)/s for a,mm,s in zip(x,mean,sd)]
    z=b+sum(wi*xi for wi,xi in zip(w,xn)); return 1/(1+math.exp(-max(-30,min(30,z))))
def auc(m,pos,neg):
    ps=[score(m,x) for x in pos]; ns=[score(m,x) for x in neg]
    w=t=0
    for a in ps:
        for c in ns:
            if a>c:w+=1
            elif a==c:t+=1
    return (w+0.5*t)/(len(ps)*len(ns))

def run(gen,label,ef=False):
    N=400
    tun=[feats(*(gen(ef) if 'entropy_fixed' in gen.__code__.co_varnames else gen())) for _ in range(N)]
    http=[feats(*https_flow()) for _ in range(N)]
    m=train(tun[:N//2],http[:N//2]); a=auc(m,tun[N//2:],http[N//2:])
    verd='INDISTINGUISHABLE' if a<0.6 else 'hard' if a<0.75 else 'DETECTABLE'
    print(f"{label:38s} AUC={a:.3f}  {verd}")
    return a

print("DPI flow-classifier: tunnel vs bulk HTTPS  (0.5=coin flip, 1.0=trivial)\n")
run(unshaped_flow,"UNSHAPED tunnel")
run(shaped_flow,"SHAPED tunnel (UDP, QUIC-ish prefix)")
run(shaped_flow,"SHAPED tunnel (TCP, inside real TLS)",ef=True)
print("\nReading:")
print("- UDP path: shaping collapses size/timing tells; entropy is the residual.")
print("- TCP path: riding inside real TLS fixes entropy too -> near coin-flip.")
