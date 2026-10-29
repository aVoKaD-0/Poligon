#!/usr/bin/env python3
"""Create deterministic Platform transport fixtures, without executing neighbour services."""
import hashlib, json, uuid
from pathlib import Path
root=Path(__file__).resolve().parent.parent/'contracts/fixtures'
def uid(label):return str(uuid.uuid5(uuid.NAMESPACE_URL,'poligon-platform-fixture:'+label))
def data(v):return (json.dumps(v,ensure_ascii=False,sort_keys=True,separators=(',',':'))+'\n').encode()
context={'schema_version':1,'project_id':uid('project'),'run_id':uid('run'),'session_id':uid('session'),'attempt_no':1}
refs={}
for kind in ['conversation','tool_calls','environment_snapshot','environment_audit']:
    record={**context,'producer_event_id':uid(kind+'-event'),'session_seq':1 if kind=='conversation' else 2,'event_kind':'agent.message','payload':{}}
    raw=data(record)
    filename=kind+'.json' if kind=='environment_snapshot' else kind+'.ndjson'
    (root/filename).write_bytes(raw)
    refs[kind]={'artifact_id':uid(kind),'sha256':hashlib.sha256(raw).hexdigest(),'size_bytes':str(len(raw)),'mime':'application/json' if filename.endswith('.json') else 'application/x-ndjson','kind':kind}
versions={k+'_version_id':uid(k+'-version') for k in ['agent','scenario','environment','evaluator','simulator']}
manifest={**context,'versions':versions,'seed':42,'execution_status':'executed','termination_reason':'completed','conversation_ref':refs['conversation'],'tool_calls_ref':refs['tool_calls'],'environment_snapshot_ref':refs['environment_snapshot'],'environment_audit_ref':refs['environment_audit'],'audio_refs':[],'usage':{'input_tokens':0,'output_tokens':0,'audio_ms':0},'timestamps':{'started_at':'2026-10-29T10:00:00Z','finished_at':'2026-10-29T10:01:00Z'}}
raw=data(manifest);(root/'execution-manifest.v1.json').write_bytes(raw)
for verdict,count in [('pass',1),('fail',2),('inconclusive',None)]:
    assessment={**context,'evaluation_id':uid('evaluation-'+verdict),'evaluator_version_id':versions['evaluator_version_id'],'manifest_sha256':hashlib.sha256(raw).hexdigest(),'verdict':verdict,'simulation_validity':'valid','assertions':[{'assertion_id':'one-active-booking','rule_id':'booking.count.v1','required':True,'status':verdict,'expected':1,'actual':count,'reason':'Transport fixture; business result is supplied by Intelligence.','evidence':[{'artifact_id':refs['environment_snapshot']['artifact_id'],'json_pointer':''}]}],'scores':[],'completeness':{'conversation':True,'tools':True,'environment':count is not None,'audio':None},'usage':{'input_tokens':0,'output_tokens':0,'provider_cost_status':'unknown'}}
    (root/('assessment-'+verdict+'.v1.json')).write_bytes(data(assessment))
