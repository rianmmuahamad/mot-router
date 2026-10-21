#!/usr/bin/env python3
"""Generate on disk, import in an isolated bank, record measured process RSS/CPU.
No proxy connections. --sizes 100000,500000,1000000 --formats uri,base64,yaml
"""
import argparse,base64,json,os,pathlib,subprocess,tempfile,time,shutil
p=argparse.ArgumentParser();p.add_argument('--engine',default='./dist/mot-nodebank');p.add_argument('--sizes',default='100000,500000,1000000');p.add_argument('--formats',default='uri,base64,yaml');p.add_argument('--output',default='benchmark-import.json');args=p.parse_args()
engine=str(pathlib.Path(args.engine).resolve());results=[]
for count in map(int,args.sizes.split(',')):
 for fmt in args.formats.split(','):
  with tempfile.TemporaryDirectory(prefix='nodebank-bench-') as tmp:
   path=pathlib.Path(tmp)/'input';data=pathlib.Path(tmp)/'bank'
   with path.open('wb') as f:
    if fmt=='yaml':f.write(b'mixed-port: 7890\nproxies:\n')
    for i in range(count):
     server=f'192.0.{i//65535%255}.{i%255+1}';password=f'p{i}'
     line=(f'  - {{name: fixture-{i}, type: http, server: {server}, port: 8123, username: user, password: {password}}}\n' if fmt=='yaml' else f'http://user:{password}@{server}:8123#fixture-{i}\n').encode()
     # Base64 independently encoded chunks divisible by three, then a final remainder.
     if fmt=='base64':
      # Streaming transform below avoids padding between individual records.
      f.write(line)
     else:f.write(line)
    # Every 500 candidates, add a duplicate and a malformed node in URI runs.
    if fmt=='uri':
     for i in range(count//500):f.write(b'http://user:p0@192.0.0.1:8123#duplicate\ninvalid://broken\n')
   if fmt=='base64':
    encoded=path.with_suffix('.b64');rem=b''
    with path.open('rb') as src,encoded.open('wb') as dest:
     while chunk:=src.read(65535):
      chunk=rem+chunk;n=len(chunk)//3*3;dest.write(base64.b64encode(chunk[:n]));rem=chunk[n:]
     if rem:dest.write(base64.b64encode(rem))
    path.unlink();path=encoded
   start=time.monotonic();proc=subprocess.Popen([engine,'--data',str(data),'import',str(path)],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True);peak=0;cpu=0
   while proc.poll() is None:
    try:
     status=pathlib.Path(f'/proc/{proc.pid}/status').read_text();peak=max(peak,int(next(l for l in status.splitlines() if l.startswith('VmRSS:')).split()[1])*1024)
     fields=pathlib.Path(f'/proc/{proc.pid}/stat').read_text().split();cpu=(int(fields[13])+int(fields[14]))/os.sysconf('SC_CLK_TCK')
    except (FileNotFoundError,StopIteration,ProcessLookupError):pass
    time.sleep(.05)
   out,err=proc.communicate();elapsed=time.monotonic()-start
   result={'nodes':count,'format':fmt,'seconds':round(elapsed,3),'rss_peak_mib':round(peak/1048576,2),'cpu_seconds':round(cpu,2),'nodes_per_second':round(count/elapsed,1),'returncode':proc.returncode,'job':json.loads(out) if proc.returncode==0 else None,'error':err[:500]}
   results.append(result);print(json.dumps(result),flush=True);pathlib.Path(args.output).write_text(json.dumps(results,indent=2))
   if proc.returncode:raise SystemExit(1)
