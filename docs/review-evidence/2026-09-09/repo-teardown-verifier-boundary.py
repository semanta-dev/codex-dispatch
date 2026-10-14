import importlib.util,tempfile,os,json
from pathlib import Path
root=Path(tempfile.mkdtemp(prefix='repo-teardown-verifier-'));repo=root/'repo';run=root/'run';repo.mkdir();run.mkdir()
s=importlib.util.spec_from_file_location('evidence',Path.cwd()/'scripts/review-evidence.py');m=importlib.util.module_from_spec(s);s.loader.exec_module(m)
os.environ['ANTHROPIC_API_KEY']='FAKE_REVIEW_CREDENTIAL_ONLY'
os.environ['CODEX_SANDBOX']='read-only'
command="python3 -c \"import os,pathlib; print(os.environ['ANTHROPIC_API_KEY']); pathlib.Path('../outside-repository.txt').write_text('sentinel')\""
r=m.check(command,repo,run,'test')
a_spec=importlib.util.spec_from_file_location('api',Path.cwd()/'scripts/api-review.py');a=importlib.util.module_from_spec(a_spec);a_spec.loader.exec_module(a)
from unittest.mock import patch,Mock
os.environ['ANTHROPIC_BASE_URL']='http://localhost:44444'
with patch.object(a,'http_request',return_value=Mock(returncode=1,stdout=b'',stderr=b'no network probe')):
 try:a.judge({'bundle':{'test':r}},root/'review')
 except ValueError:pass
print('fake_credential_in_API_request_artifact:', 'FAKE_REVIEW_CREDENTIAL_ONLY' in (root/'review/request.json').read_text())
print(root); print(json.dumps({'sandbox':os.environ['CODEX_SANDBOX'],'verification_exit':r['exit_code'],'fake_credential_in_evidence':r['stdout'].strip()=='FAKE_REVIEW_CREDENTIAL_ONLY','outside_repository_write':(root/'outside-repository.txt').exists()},indent=2))
