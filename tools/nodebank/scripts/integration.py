#!/usr/bin/env python3
"""Isolated local TLS/HTTP CONNECT fixtures, API recovery and combined RSS measurement.
Requires built dist binaries and openssl. Never changes the production bank or services.
"""
import argparse,base64,http.server,json,os,pathlib,select,signal,socket,sqlite3,ssl,subprocess,tempfile,threading,time,urllib.request,urllib.error
p=argparse.ArgumentParser();p.add_argument('--engine',default='./dist/mot-nodebank');p.add_argument('--worker',default='./dist/nodebank-probe');p.add_argument('--output',default='benchmark-runtime.json');args=p.parse_args();engine=str(pathlib.Path(args.engine).resolve());worker=str(pathlib.Path(args.worker).resolve());report={};healthy_endpoint=True;delay=.01;connections=0;peak_connections=0;conn_lock=threading.Lock()
class Target(http.server.BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_GET(self):
  if self.path=='/health':time.sleep(delay);self.send_response(204 if healthy_endpoint else 503);self.end_headers()
  else:self.send_response(200);self.send_header('Content-Length','11');self.end_headers();self.wfile.write(b'203.0.113.9')
class Proxy(http.server.BaseHTTPRequestHandler):
 def log_message(self,*a):pass
 def do_CONNECT(self):
  global connections,peak_connections
  auth=self.headers.get('Proxy-Authorization','')
  try:cred=base64.b64decode(auth.removeprefix('Basic ')).decode()
  except Exception:cred=''
  if not cred.endswith(':correct'):self.send_response(407);self.end_headers();return
  try:
   host,port=self.path.rsplit(':',1);up=socket.create_connection((host,int(port)),3)
  except OSError:self.send_response(502);self.end_headers();return
  self.send_response(200);self.end_headers();self.wfile.flush()
  with conn_lock:connections+=1;peak_connections=max(peak_connections,connections)
  try:
   streams=[self.connection,up]
   while True:
    ready,_,_=select.select(streams,[],[],16)
    if not ready:break
    for src in ready:
     b=src.recv(16384)
     if not b:return
     (up if src is self.connection else self.connection).sendall(b)
  except (OSError,ConnectionError):pass
  finally:
   up.close()
   with conn_lock:connections-=1
class Server(http.server.ThreadingHTTPServer):
 daemon_threads=True
 request_queue_size=256

def port():
 with socket.socket() as s:s.bind(('127.0.0.1',0));return s.getsockname()[1]
def wait(fn,timeout=30):
 until=time.monotonic()+timeout
 while time.monotonic()<until:
  try:
   value=fn()
   if value:return value
  except (OSError,urllib.error.URLError,KeyError):pass
  time.sleep(.08)
 raise AssertionError('Timed out waiting for condition')
def child_pids(pid):
 out=set()
 try:
  for tid in pathlib.Path(f'/proc/{pid}/task').iterdir():out.update(map(int,(tid/'children').read_text().split()))
 except OSError:pass
 return sorted(out)
def rss(pid):
 try:return int(next(l for l in pathlib.Path(f'/proc/{pid}/status').read_text().splitlines() if l.startswith('VmRSS:')).split()[1])*1024
 except (OSError,StopIteration):return 0
with tempfile.TemporaryDirectory(prefix='nodebank-integration-') as tmp:
 root=pathlib.Path(tmp);cert=root/'ca.pem';key=root/'key.pem';data=root/'data';bindport=port();base=f'http://127.0.0.1:{bindport}'
 subprocess.run(['openssl','req','-x509','-newkey','rsa:2048','-nodes','-keyout',str(key),'-out',str(cert),'-days','1','-subj','/CN=127.0.0.1','-addext','subjectAltName=IP:127.0.0.1','-addext','basicConstraints=critical,CA:TRUE'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 leaf=root/'leaf.pem';leafkey=root/'leaf.key';csr=root/'leaf.csr';ext=root/'leaf.ext';ext.write_text('subjectAltName=IP:127.0.0.1\nbasicConstraints=critical,CA:FALSE\nkeyUsage=digitalSignature,keyEncipherment\nextendedKeyUsage=serverAuth\n')
 subprocess.run(['openssl','req','-new','-newkey','rsa:2048','-nodes','-keyout',str(leafkey),'-out',str(csr),'-subj','/CN=127.0.0.1'],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 subprocess.run(['openssl','x509','-req','-in',str(csr),'-CA',str(cert),'-CAkey',str(key),'-CAcreateserial','-out',str(leaf),'-days','1','-extfile',str(ext)],check=True,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL)
 tls=Server(('127.0.0.1',0),Target);ctx=ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER);ctx.load_cert_chain(leaf,leafkey);tls.socket=ctx.wrap_socket(tls.socket,server_side=True);proxy=Server(('127.0.0.1',0),Proxy)
 for srv in [tls,proxy]:threading.Thread(target=srv.serve_forever,daemon=True).start()
 endpoint=f'https://127.0.0.1:{tls.server_port}'
 def launch():return subprocess.Popen([engine,'--data',str(data),'serve','--bind',f'127.0.0.1:{bindport}','--worker',worker,'--health-url',endpoint+'/health','--exit-url',endpoint+'/ip','--check-ca',str(cert)],stdout=subprocess.DEVNULL,stderr=subprocess.PIPE)
 def api(path,body=None,headers=None):
  h=headers or {};payload=json.dumps(body).encode() if body is not None else None
  if payload is not None:h['Content-Type']='application/json'
  with urllib.request.urlopen(urllib.request.Request(base+'/api/'+path,data=payload,headers=h),timeout=10) as r:return json.load(r)
 def upload(content):
  boundary='NODEBANK_FIXTURE_BOUNDARY';payload=(f'--{boundary}\r\nContent-Disposition: form-data; name="file"; filename="fixture.txt"\r\nContent-Type: text/plain\r\n\r\n'.encode()+content+f'\r\n--{boundary}--\r\n'.encode())
  req=urllib.request.Request(base+'/api/upload',payload,{'Content-Type':'multipart/form-data; boundary='+boundary})
  with urllib.request.urlopen(req,timeout=20) as r:return json.load(r)['job_id']
 def fixture(n,offset=0):return ''.join(f'http://u{i}:correct@127.0.0.1:{proxy.server_port}#local-{i}\n' for i in range(offset,offset+n)).encode()
 proc=launch();samples=[];latencies=[];sampling=True
 def sample():
  while sampling:
   try:
    children=child_pids(proc.pid);worker_pid=children[0] if children else 0;total=rss(proc.pid)+rss(worker_pid)
    st=pathlib.Path(f'/proc/{proc.pid}/stat').read_text().split();cpu=(int(st[13])+int(st[14]))/os.sysconf('SC_CLK_TCK')
    if worker_pid:st=pathlib.Path(f'/proc/{worker_pid}/stat').read_text().split();cpu+=(int(st[13])+int(st[14]))/os.sysconf('SC_CLK_TCK')
    samples.append((time.monotonic(),total,cpu))
   except (OSError,ValueError):pass
   time.sleep(.05)
 threading.Thread(target=sample,daemon=True).start()
 try:
  wait(lambda:api('stats').get('paused') is True);assert all(not s['enabled'] for s in api('sources')['sources']);report['startup_paused_and_sources_disabled']=True
  try:api('scan',{'background':False},{'Origin':'https://untrusted.invalid'});raise AssertionError('cross-site POST accepted')
  except urllib.error.HTTPError as e:assert e.code==403
  report['cross_site_rejected']=True
  job=upload(fixture(500));wait(lambda:api('jobs/'+job)['state']=='completed');scan=api('scan',{'background':False})['job_id'];start=time.monotonic()
  while api('jobs/'+scan)['state']!='completed':
   t=time.monotonic();api('stats');latencies.append((time.monotonic()-t)*1000);time.sleep(.1)
   assert time.monotonic()-start<60,api('stats')
  elapsed=time.monotonic()-start;wait(lambda:api('stats')['counts']['healthy']==500,15);report['local_checks']={'nodes':500,'seconds':round(elapsed,2),'nodes_per_second':round(500/elapsed,2)}
  export=api('export',{})['job_id'];wait(lambda:api('jobs/'+export)['state']=='completed');value=json.loads((data/'healthy-v1.json').read_text());assert len(value['nodes'])==1 and value['nodes'][0]['config']['password']=='correct' and value['vantage']=='laptop';assert (data/'healthy-v1.json').stat().st_mode&0o777==0o600;assert len(api('nodes?limit=1000')['nodes'])==200;report['export_ip_dedup_config_permissions_pagination']=True
  # Pause/resume during an import, and prove metadata requests stay responsive.
  job=upload(fixture(100000,1000));wait(lambda:api('jobs/'+job)['processed']>=500);api('jobs/'+job+'/control',{'action':'pause'});time.sleep(.4);n=api('jobs/'+job)['processed'];time.sleep(.4);assert api('jobs/'+job)['processed']==n;api('jobs/'+job+'/control',{'action':'resume'});wait(lambda:api('jobs/'+job)['state']=='completed',120);report['import_pause_resume']=True
  # Offline endpoint must pause without converting newly imported nodes into failed.
  healthy_endpoint=False;scan=api('scan',{'background':False})['job_id'];wait(lambda:api('stats')['paused']);assert api('stats')['counts']['failed']==0;api('jobs/'+scan+'/control',{'action':'cancel'});healthy_endpoint=True;report['endpoint_failure_preserves_bank']=True
  # Keep only a small due fixture for runtime interruption tests.
  conn=sqlite3.connect(data/'bank.db');conn.execute("UPDATE nodes SET next_check=? WHERE status='pending'",(int(time.time())+3600,));conn.commit();conn.close();job=upload(fixture(80,200000));wait(lambda:api('jobs/'+job)['state']=='completed');delay=.15
  scan=api('scan',{'background':False})['job_id'];wait(lambda:not api('stats')['paused']);api('jobs/'+scan+'/control',{'action':'pause'});wait(lambda:api('stats')['paused']);api('jobs/'+scan+'/control',{'action':'resume'});report['scan_pause_resume']=True
  # Kill only this fixture's worker while it owns leased jobs.
  pid=wait(lambda:child_pids(proc.pid))[0];os.kill(pid,signal.SIGKILL);wait(lambda:'Worker' in api('stats')['pause_reason']);api('jobs/'+scan+'/control',{'action':'resume'});wait(lambda:api('jobs/'+scan)['state']=='completed',60);report['worker_crash_requeues']=True
  # Start a stalled worker, terminate engine, then recover its leases/jobs.
  job=upload(fixture(30,300000));wait(lambda:api('jobs/'+job)['state']=='completed');delay=.5;scan=api('scan',{'background':False})['job_id'];time.sleep(.3);proc.kill();proc.wait();proc=launch();wait(lambda:api('stats').get('paused') is True);assert api('jobs/'+scan)['state']=='paused';conn=sqlite3.connect(data/'bank.db');assert conn.execute('SELECT COUNT(*) FROM nodes WHERE lease_until<>0').fetchone()[0]==0;conn.close();api('jobs/'+scan+'/control',{'action':'resume'});wait(lambda:api('jobs/'+scan)['state']=='completed',60);report['engine_crash_recovers']=True
  report['combined_rss_peak_mib']=round(max(s[1] for s in samples)/1048576,2);report['stats_request_max_ms']=round(max(latencies),2);report['peak_fixture_tunnels']=peak_connections;report['cpu_seconds_initial_process']=round(max(s[2] for s in samples),2);assert report['combined_rss_peak_mib']<256;assert report['stats_request_max_ms']<2000
 finally:
  sampling=False;proc.terminate()
  try:proc.wait(timeout=20)
  except subprocess.TimeoutExpired:proc.kill();proc.wait()
  tls.shutdown();proxy.shutdown()
pathlib.Path(args.output).write_text(json.dumps(report,indent=2)+'\n');print(json.dumps(report,indent=2))
