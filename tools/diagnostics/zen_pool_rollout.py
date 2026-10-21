#!/usr/bin/env python3
"""Run via SSH on VPS. Bounded samples; never retry a quota/access denial."""
import json,time,urllib.request,urllib.error,pathlib,datetime,concurrent.futures
BASE='http://127.0.0.1:8787'

def api(path,method='GET',data=None,timeout=5):
 body=None if data is None else json.dumps(data).encode()
 req=urllib.request.Request(BASE+path,data=body,method=method,headers={'Content-Type':'application/json'})
 with urllib.request.urlopen(req,timeout=timeout) as response:return json.load(response)

def resources():
 out={}
 for label,service in [('router','mot-router'),('runtime','mot-nodebank-runtime')]:
  import subprocess
  pid=int(subprocess.check_output(['systemctl','show',service,'-p','MainPID','--value']))
  stat=pathlib.Path(f'/proc/{pid}/status').read_text()
  for name in ['VmRSS','VmHWM']:
   for line in stat.splitlines():
    if line.startswith(name+':'):out[label+'_'+name+'_kib']=int(line.split()[1])
 return out

report={'started_at':datetime.datetime.now(datetime.timezone.utc).isoformat(),'before':resources(),'phases':[]}
# Each is an explicit small Playground acceptance sample, never a replay of
# customer traffic. Native pool probes themselves do not activate model use.
models=[(1,'muse-spark-1.3-contributor-free'),(3,'deepseek-v4.1-flash-free'),(10,'glm-5.3-flash-free')]
for parallel,model in models:
 phase={'parallelism':parallel,'model':model}
 before=api('/api/zen-pool');count=len(before.get('audit') or [])
 api('/api/zen-pool','PUT',{'enabled':True,'parallelism':parallel})
 phase['before']=resources()
 # Trigger through the same route as the user, once. A finite diagnostic body
 # contains no credential or customer prompt and only asks for a fixed marker.
 try:
  with concurrent.futures.ThreadPoolExecutor(max_workers=1) as executor:
   future=executor.submit(api,'/api/playground','POST',{'model':'opencode-zen/'+model,'prompt':'Reply with exactly ZEN_OK. Do not call tools.'},30)
   samples=[];deadline=time.monotonic()+32
   while not future.done() and time.monotonic()<deadline:
    samples.append(resources());time.sleep(.25)
   result=future.result(timeout=2)
  phase['sample_completed']=result.get('text','').strip()=='ZEN_OK'
  phase['sample_usage']=result.get('usage')
  # Do not retain raw upstream error content (it may contain untrusted data).
  if result.get('error'):phase['sample_error_present']=True
 except Exception as exc:
  phase['sample_completed']=False;phase['sample_error_class']=type(exc).__name__
 deadline=time.monotonic()+18
 while time.monotonic()<deadline:
  state=api('/api/zen-pool');samples.append(resources())
  entry=state.get('models',{}).get(model,{})
  if (entry.get('reason') or len(state.get('audit') or [])>count) and not state.get('batch'):break
  time.sleep(.25)
 phase['budget_used']=state.get('budget_used')
 phase['ready']=[x for x in state.get('ready',[]) if x['model']==model]
 phase['results']=[x for x in (state.get('audit') or [])[count:] if x.get('model')==model]
 phase['policy']=state.get('models',{}).get(model,{})
 phase['peak_rss_kib']=max((s.get('router_VmRSS_kib',0)+s.get('runtime_VmRSS_kib',0) for s in samples),default=0)
 phase['after']=resources();report['phases'].append(phase)
 print(json.dumps({'phase':parallel,'model':model,'sample_completed':phase['sample_completed'],'ready':len(phase['ready']),'policy':phase['policy'].get('reason'),'budget_used':phase['budget_used'],'peak_rss_kib':phase['peak_rss_kib']}),flush=True)
 # A policy/usage rejection terminates the rollout sample sequence. No other
 # model/IP is tested to get around it.
 if phase['policy'].get('reason') or not phase['sample_completed'] or not phase['ready']:break
# Keep the last successful rollout stage when upstream prevents validation.
# Policy cooldowns, budget, rotation settings and manual pauses survive.
validated=[x['parallelism'] for x in report['phases'] if x['sample_completed'] and x['ready'] and not x['policy'].get('reason')]
final_parallel=max(validated,default=1)
api('/api/zen-pool','PUT',{'enabled':True,'parallelism':final_parallel})
report['after']=resources();report['final']=api('/api/zen-pool')
path=pathlib.Path('/tmp/mot-zen-pool-rollout.json');path.write_text(json.dumps(report,indent=2));path.chmod(0o600)
print('Report: /tmp/mot-zen-pool-rollout.json',flush=True)
