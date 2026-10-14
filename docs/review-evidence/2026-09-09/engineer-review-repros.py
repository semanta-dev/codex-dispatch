import importlib.util, json, os, pathlib, sys
R=pathlib.Path('/home/jdziat/Code/jdziat/semanta/codex-dispatch')
def mod(name,path):
 s=importlib.util.spec_from_file_location(name,path); m=importlib.util.module_from_spec(s);s.loader.exec_module(m);return m
c=mod('tests_compact',R/'tests/python/test_compact_profile.py')
t=c.StructuredReportTests();t.setUp()
try:
 p=t.repo/'hello.txt';p.write_text('reviewed\n');t.bundle['changed_file_facts']={'hello.txt':{'kind':'file','mode':420,'sha256':__import__('hashlib').sha256(p.read_bytes()).hexdigest()}};t.persist()
 p.write_text('UNREVIEWED CHANGE\n');print('post-review live mutation accepted:',c.PROFILE.validate_result(t.final,t.prompt)['verdict'])
 p.unlink();print('post-review deletion accepted:',c.PROFILE.validate_result(t.final,t.prompt)['verdict'])
finally:t.doCleanups()
e=mod('tests_evidence',R/'tests/python/test_review_evidence.py')
t=e.ReviewEvidenceTests();t.setUp()
try:
 module=t.repo/'module';module.mkdir();os.environ.update(CODEX_WORKDIR=str(module),REVIEW_TEST_CMD='pwd',REVIEW_VERIFY_CMD='pwd')
 b=e.MODULE.collect();print('CODEX_WORKDIR:',module,'test actual cwd:',b['test']['stdout'].strip())
 (t.run/'review-evidence.json').unlink();t.result['exit_code']=1;(t.run/'result.json').write_text(json.dumps(t.result));b=e.MODULE.collect();print('failed bundle complete:',b['complete'],'persisted evidence exists:',(t.run/'review-evidence.json').exists())
 a=mod('api',R/'scripts/api-review.py'); receipt=t.repo/'receipt.json';identity={'id':'test'};receipt.with_suffix('.runs.json').write_text(json.dumps({'identity':identity,'attempts':[{'state':'complete','iteration':1,'resume_session':'','run_dir':str(t.run)}]}))
 saved={'report':{'kind':'review','verdict':'fail'},'payload':{'receipt_path':str(receipt)},'identity':identity,'config':{'max_iter':1,'no_resume':False}}
 try:a.validate_history(saved)
 except Exception as x:print('actual API validate_history failed:',type(x).__name__,str(x))
finally:t.doCleanups()
