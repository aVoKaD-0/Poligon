#!/usr/bin/env python3
"""Generate the committed OpenAPI contract and its TypeScript transport client."""
import json
from pathlib import Path
ROOT = Path(__file__).resolve().parent.parent
schemas = {}
def ref(name): return {'$ref': '#/components/schemas/' + name}
def obj(props, required=None, extra=False):
    return {'type': 'object', 'properties': props, 'required': list(props) if required is None else required, 'additionalProperties': extra}
def arr(item): return {'type': 'array', 'items': item}
S = {'type': 'string'}
U = {'type': 'string', 'format': 'uuid'}
T = {'type': 'string', 'format': 'date-time'}
N = {'type': 'integer'}
B = {'type': 'boolean'}
J = {'type': 'object', 'additionalProperties': True}
NULL = {'type': 'null'}
def nullable(x): return {'anyOf': [x, NULL]}
def enum(*values): return {'type': 'string', 'enum': list(values)}
for name, filename in [('AgentDefinition','agent'), ('ScenarioDefinition','scenario'), ('ArtifactRef','artifact-ref')]:
    schemas[name] = json.loads((ROOT/'contracts/schemas'/f'{filename}.json').read_text())
    schemas[name].pop('$schema',None)
schemas['APIError'] = obj({'error':obj({'code':S,'message':S,'request_id':U,'details':J})})
schemas['Project'] = obj({'id':U,'name':S,'revision':S,'concurrency':N,'created_at':T})
schemas['ProjectInput'] = obj({'name':{'type':'string','minLength':1,'maxLength':200}})
schemas['Member'] = obj({'id':U,'user_id':U,'role':enum('owner','editor','viewer'),'name':S})
schemas['MemberInput'] = obj({'user_id':U,'role':enum('owner','editor','viewer')})
schemas['RoleInput'] = obj({'role':enum('owner','editor','viewer')})
schemas['Resource'] = obj({'id':U,'project_id':U,'kind':enum('agent','scenario'),'title':S,'archived':B,'revision':S,'dataset_item_id':nullable(U),'created_at':T})
schemas['ResourceInput'] = obj({'title':{'type':'string','minLength':1,'maxLength':200}})
schemas['ResourcePatch'] = obj({'title':{'type':'string','minLength':1,'maxLength':200},'archived':B},[])
schemas['ResourcePatch']['minProperties']=1
schemas['Version'] = obj({'id':U,'project_id':nullable(U),'parent_id':nullable(U),'kind':enum('agent','scenario','environment','evaluator','simulator'),'version_number':N,'status':enum('draft','published'),'revision':S,'definition':J,'definition_sha256':nullable(S),'created_at':T,'published_at':nullable(T)})
schemas['AgentVersionInput'] = obj({'definition':J})
schemas['ScenarioVersionInput'] = obj({'definition':J})
schemas['Credential'] = obj({'id':U,'project_id':U,'label':S,'created_at':T,'revoked_at':nullable(T)},['id','project_id','label'])
schemas['CredentialInput'] = obj({'label':{'type':'string','minLength':1,'maxLength':200},'value':{'type':'string','minLength':1,'maxLength':16384}})
schemas['RunInput'] = obj({'agent_version_id':U,'scenario_version_ids':dict(arr(U),minItems=1,maxItems=100,uniqueItems=True),'evaluator_version_id':U,'repeats':{'type':'integer','minimum':1,'maximum':10},'seed':{'type':'integer','minimum':-9007199254740991,'maximum':9007199254740991},'concurrency':{'type':'integer','minimum':1,'maximum':20}})
verdict = nullable(enum('pass','fail','inconclusive'))
schemas['Run'] = obj({'id':U,'project_id':U,'status':enum('queued','running','cancelling','completed','cancelled','failed'),'verdict':verdict,'agent_version_id':U,'evaluator_version_id':U,'scenario_version_ids':arr(U),'session_count':N,'seed':N,'repeats':N,'counts_by_status':{'type':'object','additionalProperties':N},'counts_by_verdict':{'type':'object','additionalProperties':N},'revision':S,'last_event_cursor':S,'cancel_requested':B,'concurrency':N,'queue_timeout_ms':N,'created_at':T,'started_at':nullable(T),'finished_at':nullable(T)})
schemas['RunAccepted'] = obj({'run_id':U,'status':S,'session_count':N,'created_at':T})
schemas['Session'] = obj({'id':U,'project_id':U,'run_id':U,'scenario_version_id':U,'dataset_item_id':U,'repetition_index':N,'seed':{'type':'integer','minimum':-9007199254740991,'maximum':9007199254740991},'revision':S,'status':enum('queued','running','evaluating','completed','cancelled','failed'),'verdict':verdict,'current_attempt_no':N,'current_evaluation_id':nullable(U),'primary_evaluation_id':nullable(U),'error':nullable(J),'completeness':J,'attempts':arr(J)},[k for k in ['id','project_id','run_id','scenario_version_id','dataset_item_id','repetition_index','seed','status','verdict','current_attempt_no','current_evaluation_id','primary_evaluation_id','error','completeness']])
schemas['Evaluation'] = obj({'id':U,'session_id':U,'attempt_no':N,'evaluation_group_id':U,'purpose':enum('primary','manual'),'retry_index':N,'current':B,'evaluator_version_id':U,'status':enum('queued','running','completed','failed','cancelled'),'cancel_requested':B,'lease_expiry':nullable(T),'deadline':nullable(T),'assessment_ref':nullable(ref('ArtifactRef')),'assessment':nullable(J),'error':nullable(J),'created_at':T})
schemas['EvaluationInput'] = obj({'evaluator_version_id':U})
schemas['EvaluationAccepted'] = obj({'evaluation_id':U,'evaluation_group_id':U,'purpose':enum('manual'),'status':enum('queued')})
schemas['Profile'] = obj({'id':U,'name':S,'csrf_token':S,'session_expires_at':T,'project_roles':arr(obj({'project_id':U,'role':enum('owner','editor','viewer')}))})
schemas['Report'] = obj({'run':ref('Run'),'sessions':arr(J),'assertion_counts':{'type':'object','additionalProperties':N},'pass_rate':obj({'passed':N,'denominator':N,'excluded':N,'inconclusive':N}),'usage':J,'artifact_refs':arr(J)})
schemas['Comparison'] = obj({'baseline_run_id':U,'candidate_run_id':U,'items':arr(obj({'case':S,'baseline':nullable(J),'candidate':nullable(J),'change':enum('unchanged','missing','excluded','improved','regressed')}))})
schemas['SessionEventsPage'] = obj({'items':arr(J),'next_cursor':nullable(S),'attempt_no':N,'completeness':J})
for name in ['Project','Member','Resource','Version','Credential','Run','Session','Evaluation']:
    schemas[name+'Page']=obj({'items':arr(ref(name)),'next_cursor':nullable(S)})
paths={}
def parameter(name,where='query',sch=S,required=False):return {'name':name,'in':where,'required':required,'schema':sch}
ERRORS={str(n):{'description':desc,'content':{'application/json':{'schema':ref('APIError')}}} for n,desc in [(400,'Invalid JSON'),(401,'Session required'),(403,'Insufficient role or CSRF rejected'),(404,'Resource not visible'),(409,'Conflict or idempotency mismatch'),(412,'If-Match is stale'),(422,'Validation or incompatible versions'),(429,'Quota exceeded'),(503,'Dependency unavailable')]}
def route(method,path,operation,response=None,request=None,status=200,etag=False,idem=False,page=False,params=(),description=''):
    full='/api/v1'+path
    parameters=[]
    import re
    for key in re.findall(r'\{(.*?)\}',path): parameters.append(parameter(key,'path',U,True))
    if method not in ['get']: parameters.append(parameter('X-CSRF-Token','header',S,True));parameters.append(parameter('Origin','header',S,True))
    if etag: parameters.append(parameter('If-Match','header',S,True))
    if idem: parameters.append(parameter('Idempotency-Key','header',{'type':'string','minLength':1,'maxLength':200},True))
    if page: parameters += [parameter('limit',sch={'type':'integer','minimum':1,'maximum':100,'default':50}),parameter('cursor')]
    parameters += list(params)
    responses={**ERRORS,str(status):{'description':'Success'}}
    if response: responses[str(status)]['content']={'application/json':{'schema':ref(response) if isinstance(response,str) else response}}
    op={'operationId':operation,'parameters':parameters,'responses':responses,'description':description,'security':[{'sessionCookie':[]}]}
    if request is not None: op['requestBody']={'required':True,'content':{'application/json':{'schema':ref(request) if isinstance(request,str) else request}}}
    paths.setdefault(full,{})[method]=op
P='/projects/{project_id}'
route('get','/auth/login','login',status=302);route('get','/auth/callback','callback',status=302,params=[parameter('code',required=True),parameter('state',required=True)])
for p in ['/api/v1/auth/login','/api/v1/auth/callback']:paths[p]['get']['security']=[]
route('post','/auth/logout','logout',J);route('get','/me','getProfile','Profile')
route('get','/projects','listProjects','ProjectPage',page=True);route('post','/projects','createProject','Project','ProjectInput',201)
route('get',P,'getProject','Project');route('patch',P,'updateProject','Project','ProjectInput',etag=True)
route('get',P+'/members','listMembers','MemberPage',page=True);route('post',P+'/members','addMember',J,'MemberInput')
route('patch',P+'/members/{user_id}','updateMember',J,'RoleInput');route('delete',P+'/members/{user_id}','deleteMember',J)
for kind in ['agents','scenarios']:
    label='Agent' if kind=='agents' else 'Scenario';base=P+'/'+kind
    route('get',base,'list'+label+'s','ResourcePage',page=True);route('post',base,'create'+label,'Resource','ResourceInput',201)
    base+='/{resource_id}';route('get',base,'get'+label,'Resource');route('patch',base,'update'+label,'Resource','ResourcePatch',etag=True)
    route('get',base+'/versions','list'+label+'Versions','VersionPage',page=True);route('post',base+'/versions','create'+label+'Version','Version',label+'VersionInput',201)
    base+='/versions/{version_id}';route('get',base,'get'+label+'Version','Version');route('patch',base,'update'+label+'Version','Version',label+'VersionInput',etag=True)
    route('post',base+'/publish','publish'+label+'Version','Version',etag=True,description='Calls external validators; does not publish when a validator is unavailable.')
for kind in ['environment','evaluator','simulator']:
    route('get',P+'/'+kind+'-versions','list'+kind.title()+'Versions','VersionPage',page=True);route('get',P+'/'+kind+'-versions/{version_id}','get'+kind.title()+'Version','Version')
route('get',P+'/credentials','listCredentials','CredentialPage',page=True);route('post',P+'/credentials','createCredential','Credential','CredentialInput',201);route('delete',P+'/credentials/{credential_id}','revokeCredential',J)
route('get',P+'/runs','listRuns','RunPage',page=True);route('post',P+'/runs','createRun','RunAccepted','RunInput',202,idem=True)
R=P+'/runs/{run_id}'
route('get',R,'getRun','Run');route('post',R+'/cancel','cancelRun','Run',status=202);route('post',R+'/retry','retryRun','RunAccepted',obj({}),202,idem=True)
route('get',R+'/sessions','listSessions','SessionPage',page=True);route('get',R+'/report','getReport','Report')
route('get',R+'/stream','streamRun',params=[parameter('Last-Event-ID','header'),parameter('after_cursor')],description='SSE replay with decimal string cursor. Events: run.updated, session.updated, session.progress, assessment.ready, stream.reset. Named heartbeat every 15 seconds without an id. Reload REST snapshot on stream.reset.')
paths['/api/v1'+R+'/stream']['get']['responses']['200']['content']={'text/event-stream':{'schema':S}}
SS=P+'/sessions/{session_id}'
route('get',SS,'getSession','Session');route('get',SS+'/events','getSessionEvents','SessionEventsPage',page=True,params=[parameter('after_seq'),parameter('attempt_no',sch={'type':'integer','minimum':1})])
route('get',SS+'/assessment','getAssessment',J);route('post',SS+'/reevaluate','reevaluate','EvaluationAccepted','EvaluationInput',202,idem=True)
route('get',SS+'/evaluations','listEvaluations','EvaluationPage',page=True);route('get',SS+'/evaluations/{evaluation_id}','getEvaluation','Evaluation');route('post',SS+'/evaluations/{evaluation_id}/cancel','cancelEvaluation',J,status=202)
route('get',P+'/artifacts/{artifact_id}/download','downloadArtifact',status=302,description='Project-authorized signed GET URL, valid for 60 seconds.')
route('get',P+'/comparisons','compareRuns','Comparison',params=[parameter('baseline_run_id',sch=U,required=True),parameter('candidate_run_id',sch=U,required=True)])
schemas['RunInput']['examples']=[{'agent_version_id':'b0000000-0000-4000-8000-000000000001','scenario_version_ids':['b0000000-0000-4000-8000-000000000002'],'evaluator_version_id':'a0000000-0000-4000-8000-000000000003','repeats':1,'seed':42,'concurrency':5}]
api={'openapi':'3.1.0','info':{'title':'Poligon Platform API','version':'1.0.0','description':'Backend 1. Draft definitions may be incomplete; publication validates the full definition. Cookies are opaque BFF sessions.'},'servers':[{'url':'/'}],'paths':paths,'components':{'securitySchemes':{'sessionCookie':{'type':'apiKey','in':'cookie','name':'poligon_session'}},'schemas':schemas}}
(ROOT/'contracts/openapi/platform.v1.json').write_text(json.dumps(api,ensure_ascii=False,indent=2)+'\n')
def ts(sch):
    if '$ref' in sch:return sch['$ref'].split('/')[-1]
    if 'anyOf' in sch:return ' | '.join(ts(x) for x in sch['anyOf'])
    if 'enum' in sch:return ' | '.join(json.dumps(x) for x in sch['enum'])
    if 'const' in sch:return json.dumps(sch['const'])
    t=sch.get('type');
    if t=='string':return 'string'
    if t in ['integer','number']:return 'number'
    if t=='boolean':return 'boolean'
    if t=='null':return 'null'
    if t=='array':return 'Array<'+ts(sch['items'])+'>'
    if t=='object':
        fields=[json.dumps(k)+('' if k in sch.get('required',[]) else '?')+': '+ts(v) for k,v in sch.get('properties',{}).items()]
        if not fields:return 'Record<string, '+(ts(sch['additionalProperties']) if isinstance(sch.get('additionalProperties'),dict) else 'unknown')+'>'
        return '{ '+ '; '.join(fields)+' }'
    return 'unknown'
lines=['// Generated by scripts/contracts.py. Do not edit.']
for name,s in schemas.items():lines.append('export type '+name+' = '+ts(s)+';')
lines += ['export type Operations = {']
for p,ops in paths.items():
    for m,op in ops.items():
        if not any(k.startswith('2') and 'application/json' in v.get('content',{}) for k,v in op['responses'].items()): continue
        request=op.get('requestBody',{}).get('content',{}).get('application/json',{}).get('schema',{})
        response=next((r.get('content',{}).get('application/json',{}).get('schema',{}) for k,r in op['responses'].items() if k.startswith('2')), {})
        pathparams=[x['name'] for x in op['parameters'] if x['in']=='path']
        lines.append('  '+op['operationId']+': { request: '+ts(request)+'; response: '+ts(response)+'; path: '+ts(obj({x:S for x in pathparams}))+' };')
lines += ['};', 'export class PlatformError extends Error { constructor(public status: number, public body: APIError) { super(body.error.message); } }',
'''export async function platformRequest<K extends keyof Operations>(operation: K, options: { path: Operations[K]['path']; body?: Operations[K]['request']; csrfToken?: string; idempotencyKey?: string; etag?: string; query?: Record<string, string>; signal?: AbortSignal }): Promise<Operations[K]['response']> {
  const spec = routes[operation];
  let path: string = spec.path;
  for (const [name, value] of Object.entries(options.path)) path = path.replace('{' + name + '}', encodeURIComponent(String(value)));
  if (options.query) path += '?' + new URLSearchParams(options.query);
  const headers: Record<string, string> = {};
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';
  if (options.csrfToken) headers['X-CSRF-Token'] = options.csrfToken;
  if (options.idempotencyKey) headers['Idempotency-Key'] = options.idempotencyKey;
  if (options.etag) headers['If-Match'] = options.etag;
  const response = await fetch(path, {method: spec.method, credentials: 'same-origin', headers, body: options.body === undefined ? undefined : JSON.stringify(options.body), signal: options.signal});
  if (!response.ok) throw new PlatformError(response.status, await response.json() as APIError);
  return await response.json() as Operations[K]['response'];
}''', 'const routes = '+json.dumps({op['operationId']:{'method':m.upper(),'path':p} for p,ops in paths.items() for m,op in ops.items() if any(k.startswith('2') and 'application/json' in v.get('content',{}) for k,v in op['responses'].items())},ensure_ascii=False,indent=2)+' as const;']
(ROOT/'contracts/openapi/client.ts').write_text('\n'.join(lines)+'\n')
print(f'Generated {len(paths)} paths, {sum(len(x) for x in paths.values())} operations, {len(schemas)} schemas')
