#!/usr/bin/env python3
"""Fixture-only tests; no production accounts or AI upstream requests."""
import concurrent.futures as futures
import http.server
import json
import os
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

binary=os.path.abspath(sys.argv[1])
lock=threading.Lock()
active={};peak={};calls=[]
held=threading.Event();started=threading.Event()
mode='normal'
class Backend(http.server.BaseHTTPRequestHandler):
 def do_POST(self):
  body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
  user=self.headers['X-Motgate-User']
  with lock:
   active[user]=active.get(user,0)+1;peak[user]=max(peak.get(user,0),active[user]);calls.append((user,body))
   if active[user]>=2:started.set()
  try:
   if mode=='held':held.wait(4)
   else:time.sleep(.25)
   if body.get('stream'):
    data=b'data: {"choices":[{"delta":{"content":"OK"}}]}\n\ndata: [DONE]\n\n'
    kind='text/event-stream'
   else:data=json.dumps({'ok':True,'model':body['model']}).encode();kind='application/json'
   self.send_response(200);self.send_header('Content-Type',kind);self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
  finally:
   with lock:active[user]-=1
 def log_message(self,*args):pass
backend=http.server.ThreadingHTTPServer(('127.0.0.1',0),Backend)
threading.Thread(target=backend.serve_forever,daemon=True).start()

def get(port):
 with urllib.request.urlopen(f'http://127.0.0.1:{port}/healthz',timeout=2) as r:return json.load(r)
def wait_for(fn):
 until=time.monotonic()+3
 while time.monotonic()<until:
  try:
   if fn():return
  except (OSError,urllib.error.URLError):pass
  time.sleep(.01)
 raise AssertionError('condition did not become true')
def launch(wait):
 with socket.socket() as sock:sock.bind(('127.0.0.1',0));port=sock.getsockname()[1]
 p=subprocess.Popen([binary,'--listen',f'127.0.0.1:{port}','--upstream',f'http://127.0.0.1:{backend.server_port}','--max-inflight','16','--per-user-inflight','2','--global-rps','1000','--user-rps','1000','--queue-wait-ms',str(wait),'--retries','0'],env={**os.environ,'GUARD_MASTER_KEY':'fixture-only'},stdout=subprocess.DEVNULL,stderr=subprocess.PIPE)
 try:wait_for(lambda:get(port)['ok']);return p,port
 except Exception:p.terminate();p.wait();raise

def request(port,user,stream=False):
 body=json.dumps({'model':'explicit-fixture-model','stream':stream}).encode()
 r=urllib.request.Request(f'http://127.0.0.1:{port}/v1/chat/completions',data=body,headers={'Content-Type':'application/json','X-Motgate-User':user})
 try:
  with urllib.request.urlopen(r,timeout=6) as response:return response.status,response.read()
 except urllib.error.HTTPError as e:return e.code,e.read()

def stop(p):p.terminate();p.wait(timeout=3)
try:
 # Burst exceeds per-user active slots but streams and JSON all finish successfully.
 p,port=launch(3000)
 try:
  with futures.ThreadPoolExecutor(max_workers=6) as pool:
   results=list(pool.map(lambda i:request(port,'burst',i%2==0),range(6)))
  assert all(code==200 for code,_ in results),results
  assert peak['burst']<=2,peak
  assert all((b'[DONE]' in raw) if i%2==0 else json.loads(raw)['model']=='explicit-fixture-model' for i,(_,raw) in enumerate(results))
  status=get(port);assert status['queued']==0 and status['waited']>=4,status
  print('burst: six requests succeeded with at most two active; SSE intact',flush=True)
 finally:stop(p)
 # A closed waiting client is never forwarded after a slot becomes available.
 mode='held';held.clear();started.clear();p,port=launch(3000)
 try:
  with futures.ThreadPoolExecutor(max_workers=3) as pool:
   first=[pool.submit(request,port,'cancel') for _ in range(2)]
   assert started.wait(2)
   sock=socket.create_connection(('127.0.0.1',port))
   body=b'{"model":"explicit-fixture-model"}'
   sock.sendall(f'POST /v1/chat/completions HTTP/1.1\r\nHost: localhost\r\nX-Motgate-User: cancel\r\nContent-Length: {len(body)}\r\n\r\n'.encode()+body)
   wait_for(lambda:get(port)['queued']==1)
   # Other accounts remain admissible while this user's queue is occupied.
   other=pool.submit(request,port,'independent')
   wait_for(lambda:active.get('independent',0)==1)
   sock.close();wait_for(lambda:get(port)['queued']==0)
   held.set();assert all(f.result()[0]==200 for f in first);assert other.result()[0]==200
  assert len([x for x in calls if x[0]=='cancel'])==2,calls
  print('cancel/isolation: disconnected waiter dropped; other account admitted',flush=True)
 finally:held.set();stop(p)
 # A genuine exhausted waiting deadline remains an explicit error, not fake success.
 mode='normal';started.clear();p,port=launch(30)
 try:
  with futures.ThreadPoolExecutor(max_workers=3) as pool:
   first=[pool.submit(request,port,'timeout') for _ in range(2)]
   assert started.wait(2)
   code,raw=request(port,'timeout')
   assert code==429 and json.loads(raw)['error']['type']=='rate_limit_error',(code,raw)
   assert all(f.result()[0]==200 for f in first)
  assert len([x for x in calls if x[0]=='timeout'])==2
  assert get(port)['queued']==0 and get(port)['queue_timeouts']==1
  print('timeout: explicit 429, no extra upstream request or slot leak',flush=True)
 finally:stop(p)
finally:backend.shutdown()
