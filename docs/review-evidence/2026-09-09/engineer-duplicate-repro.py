import argparse,importlib.util,pathlib,sys,tempfile
from unittest.mock import patch
s=importlib.util.spec_from_file_location('runner','/home/jdziat/Code/jdziat/semanta/codex-dispatch/scripts/graphrag-plan-runner.py');m=importlib.util.module_from_spec(s);sys.modules[s.name]=m;s.loader.exec_module(m)
with tempfile.TemporaryDirectory() as d:
 p=pathlib.Path(d);repo=p/'repo';repo.mkdir();plan=p/'plan.md'
 plan.write_text('\n'.join(f'''## Packet {n}: {title}\nAllowed files:\n- {title}.txt\n- {title}.done.md\nAcceptance criteria:\n- implement {title}\nVerification:\ntrue\nProgress record:\n{title}.done.md\n''' for n,title in [('1','first'),('001','second')]))
 args=argparse.Namespace(repo=str(repo),out=str(p/'out'),plan=str(plan),shared_broker=False,shared_broker_addr='',rerun=True,isolation='none',jobs=1)
 executed=[]
 def run(packet,*args):executed.append(packet.title);return {'packet':packet.number,'status':'pass'}
 with patch.object(m,'run_packet',side_effect=run):rc=m.run_plan(args)
 print('exit:',rc,'executed:',executed)
