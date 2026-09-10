from pathlib import Path
import tempfile,subprocess,os,shutil,hashlib,tarfile,json
root=Path(tempfile.mkdtemp(prefix='repo-teardown-launcher-')); source=Path.cwd()
(root/'scripts/hooks').mkdir(parents=True)
for file in ['scripts/dispatch-codex.sh','scripts/hooks/stop.sh','scripts/hooks/session-end.sh','scripts/hooks/session-start.sh']:
 shutil.copy2(source/file,root/file)
(root/'VERSION').write_text('0.99.0-review\n')
cache=root/'cache/codex-dispatch/v0.99.0-review'; cache.mkdir(parents=True)
exe=cache/'codex-dispatch.exe';exe.write_text('#!/bin/bash\nprintf "HOOK_CALLED\\n"\n');exe.chmod(0o755)
env={**os.environ,'XDG_CACHE_HOME':str(root/'cache')};env.pop('CODEX_DISPATCH_BIN',None)
results={}
for name in ['session-start','stop','session-end']:
 r=subprocess.run(['bash',str(root/f'scripts/hooks/{name}.sh')],input='{}',text=True,capture_output=True,env=env);results['windows_cache_'+name]={'exit':r.returncode,'out':r.stdout.strip()}
exe.rename(cache/'codex-dispatch')
r=subprocess.run(['bash',str(root/'scripts/hooks/stop.sh')],input='{}',text=True,capture_output=True,env=env);results['extensionless_control']={'exit':r.returncode,'out':r.stdout.strip()}
shutil.rmtree(root/'cache');release=root/'release';release.mkdir();stage=root/'stage';stage.mkdir()
b=stage/'codex-dispatch';b.write_text('#!/bin/bash\nprintf "DISPATCH_OK\\n"\n');b.chmod(0o755)
archive=release/'codex-dispatch_linux-amd64.tar.gz'
with tarfile.open(archive,'w:gz') as out:out.add(b,arcname='codex-dispatch')
(release/'checksums.txt').write_text(hashlib.sha256(archive.read_bytes()).hexdigest()+'  '+archive.name+'\n')
shim=root/'path';shim.mkdir()
for name in ['gzip','bash','sh','dirname','uname','cat','tar','curl','sha256sum','awk','grep','mktemp','mkdir','rm','rmdir','chmod','sleep']:
 p=shutil.which(name)
 if p:(shim/name).symlink_to(p)
env.update(PATH=str(shim),CODEX_DISPATCH_RELEASE_URL=release.as_uri())
r=subprocess.run(['/bin/bash',str(root/'scripts/dispatch-codex.sh')],capture_output=True,text=True,env=env,timeout=10)
results['no_flock_first_install']={'exit':r.returncode,'out':r.stdout,'err':r.stderr,'sentinel_left':(cache/'.lock.d').exists()}
(root/'results.json').write_text(json.dumps(results,indent=2));print(root);print(json.dumps(results,indent=2))
