#!/usr/bin/env python3
"""Generate secrets for an isolated local development profile, without printing them."""
import base64, os, secrets
from pathlib import Path
root=Path(__file__).resolve().parent.parent
path=root/'.env'
values={
 'APP_ENV':'local','HTTP_ADDR':':8080','GRPC_ADDR':':9090',
 'PUBLIC_ORIGIN':'http://localhost:8080',
 'POSTGRES_PASSWORD':secrets.token_urlsafe(32),
 'NATS_PLATFORM_PASSWORD':secrets.token_urlsafe(32),
 'NATS_RUNNER_PASSWORD':secrets.token_urlsafe(32),
 'NATS_INTELLIGENCE_PASSWORD':secrets.token_urlsafe(32),
 'RUNNER_SERVICE_TOKEN':secrets.token_urlsafe(32),
 'ENVIRONMENT_SERVICE_TOKEN':secrets.token_urlsafe(32),
 'INTELLIGENCE_SERVICE_TOKEN':secrets.token_urlsafe(32),
 'PLATFORM_SERVICE_TOKEN':secrets.token_urlsafe(32),
 'CREDENTIAL_MASTER_KEY':base64.b64encode(secrets.token_bytes(32)).decode(),
 'S3_ACCESS_KEY':'platform-'+secrets.token_hex(8),
 'S3_SECRET_KEY':secrets.token_urlsafe(32),
 'S3_BUCKET':'poligon-evidence','S3_ENDPOINT':'localhost:9000','S3_PUBLIC_ENDPOINT':'localhost:9000',
 'ENVIRONMENT_GRPC_ADDR':'localhost:9091','INTELLIGENCE_GRPC_ADDR':'localhost:9092',
 'ALLOW_PRIVATE_AGENTS':'true','AGENT_ALLOWED_HOSTS':'localhost,host.docker.internal',
 'KEYCLOAK_ADMIN':'admin','KEYCLOAK_ADMIN_PASSWORD':secrets.token_urlsafe(32),
 'OIDC_CLIENT_ID':'poligon-platform','OIDC_CLIENT_SECRET':'',
 'METRICS_TOKEN':secrets.token_urlsafe(32)
}
values['DATABASE_URL']='postgres://platform:'+values['POSTGRES_PASSWORD']+'@localhost:5432/platform_db?sslmode=disable'
values['NATS_URL']='nats://localhost:4222'
values['NATS_USER']='platform'
values['NATS_PASSWORD']=values['NATS_PLATFORM_PASSWORD']
fd=os.open(path,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
with os.fdopen(fd,'w') as f:f.write(''.join(k+'='+v+'\n' for k,v in values.items()))
print('Created .env with fresh local secrets. Existing .env is never overwritten.')
