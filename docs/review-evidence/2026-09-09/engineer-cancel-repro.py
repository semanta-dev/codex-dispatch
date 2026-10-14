import importlib.util, os, pathlib, signal, subprocess, sys, tempfile, time
R='/home/jdziat/Code/jdziat/semanta/codex-dispatch/scripts/hooks/codex-expansion.py'
if len(sys.argv)>1:
 spec=importlib.util.spec_from_file_location('hook',R);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
 m.invoke(['bash','-c','echo $$ > "$1"; sleep 1; echo still-running > "$2"','bash',sys.argv[1],sys.argv[2]],os.environ,'/tmp');sys.exit()
with tempfile.TemporaryDirectory() as d:
 pidfile=pathlib.Path(d)/'pid';sentinel=pathlib.Path(d)/'sentinel'
 p=subprocess.Popen([sys.executable,__file__,str(pidfile),str(sentinel)])
 for _ in range(100):
  if pidfile.exists():break
  time.sleep(.02)
 p.send_signal(signal.SIGINT);p.wait(timeout=3)
 time.sleep(1.2)
 print('hook interrupted exit:',p.returncode,'child completed mutation after interrupt:',sentinel.exists())
