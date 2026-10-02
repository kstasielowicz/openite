import re,subprocess,os,sys
s=open('web/index.html',encoding='utf8').read()
bad=0
for i,b in enumerate(re.findall(r'<script>(.*?)</script>',s,re.S)):
    open(f'blk{i}.js','w',encoding='utf8').write(b)
    r=subprocess.run(['node','--check',f'blk{i}.js'],capture_output=True,text=True)
    if r.returncode: print(i, r.stderr[:500]); bad=1
    os.remove(f'blk{i}.js')
print("ok" if not bad else "FAIL")
