#!/usr/bin/env python3
"""Back up Platform DB and S3 objects. Requires pg_dump and configured mc alias."""
import argparse, os, subprocess
from pathlib import Path
from urllib.parse import urlparse, parse_qs, unquote
p=argparse.ArgumentParser();p.add_argument('directory');p.add_argument('--s3-source',required=True,help='mc alias/bucket configured by the operator');args=p.parse_args()
url=urlparse(os.environ['DATABASE_URL']);env=os.environ.copy()
for key,value in {'PGHOST':url.hostname,'PGPORT':str(url.port or 5432),'PGUSER':unquote(url.username or ''),'PGPASSWORD':unquote(url.password or ''),'PGDATABASE':unquote(url.path.lstrip('/')),'PGSSLMODE':parse_qs(url.query).get('sslmode',['prefer'])[0]}.items():
    if value:env[key]=value
folder=Path(args.directory);folder.mkdir(mode=0o700,parents=True,exist_ok=False)
subprocess.run(['pg_dump','--format=custom','--no-owner','--file',str(folder/'platform.dump')],env=env,check=True)
subprocess.run(['mc','mirror',args.s3_source,str(folder/'objects')],check=True)
print('Backup complete. Store the credential master key separately. Verify a restore before treating the backup as usable.')
