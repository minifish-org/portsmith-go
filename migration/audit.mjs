import { mkdtemp, mkdir, readFile, writeFile, rm, cp } from "node:fs/promises";
import path from "node:path";
import { tmpdir } from "node:os";
import { pathToFileURL } from "node:url";
import { execFile } from "node:child_process";
import { promisify } from "node:util";
import { hydrate, project, executor, material, toolEnvironment } from "./hydrate.mjs";
const exec=promisify(execFile);
const {inspectModules}=await import(pathToFileURL(path.join(executor,"dist/modules.js")));
const {prepareTask,loadTask}=await import(pathToFileURL(path.join(executor,"dist/workspace.js")));
const {verifyPort}=await import(pathToFileURL(path.join(executor,"dist/verify.js")));
await hydrate();
const env=toolEnvironment();Object.assign(process.env,env);
const inspected=await inspectModules(material);
const temp=await mkdtemp(path.join(tmpdir(),"portsmith-go-material-audit-"));
const put=async(root,name,data)=>{const p=path.join(root,name);await mkdir(path.dirname(p),{recursive:true});await writeFile(p,data);};
const run=async(cwd,args)=>exec("go",args,{cwd,env:{...env,CGO_ENABLED:"0"},maxBuffer:32*1024*1024,timeout:300000});
const started=new Date().toISOString();
try{
 const probes={};
 for(const name of ["compiler","pith"]){
  const d=path.join(temp,"probe-"+name);await put(d,"go.mod",inspected.mod);await put(d,"go.sum",inspected.sum);
  await put(d,"main.go",await readFile(path.join(material,"probes",name+".go.txt")));
  const args=name==="compiler"?[path.join(material,"assets/typescript.txt"),path.join(material,"judges/verification/internal/portsmith/testdata/oracle/event-stream.txt"),path.join(material,"judges/verification/internal/portsmith/testdata/oracle/expected.json")]:[];
  const result=await run(d,["run","-mod=readonly","-p=4",".",...args]);probes[name]=JSON.parse(result.stdout);console.log(`${name} compatibility probe passed.`);
 }
 const compile=path.join(temp,"compile");await put(compile,"go.mod",inspected.mod);await put(compile,"go.sum",inspected.sum);
 const judges=[],required=[],assets=[],writable=[];
 for(const [i,item] of inspected.items.entries()){
  judges.push(...item.judge);required.push(...item.spec.tests);assets.push(...item.assets);writable.push(...item.spec.outputs);
  for(const f of item.judge)await put(compile,f.name,f.data);
  for(const f of item.assets)await put(compile,f.name,f.data);
  const taskRoot=path.join(temp,"tasks",String(i));
  await prepareTask({source:inspected.source,out:taskRoot,files:item.spec.sources,revision:inspected.p.revision,goal:item.spec.goal,contract:item.contract,rules:path.join(material,"RULEBOOK.md"),goMod:path.join(project,"go.mod"),goSum:path.join(project,"go.sum"),judgeFiles:judges,requiredJudgeTests:required,seed:assets,writableFiles:[...new Set(writable)],moduleTask:true});
  await loadTask(taskRoot);
 }
 const stub=await readFile(path.join(material,"API.go.txt"));
 const main='package main\nimport("context";"os";ps "github.com/minifish-org/portsmith-go/internal/portsmith")\nfunc main(){os.Exit(ps.Main(context.Background(),os.Args[1:],os.Stdout,os.Stderr))}\n';
 await put(compile,"internal/portsmith/files.go",stub);await put(compile,"cmd/portsmith/main.go",main);
 await run(compile,["test","-mod=readonly","-run","^$","./..."]);
 let output;
 try{await run(compile,["test","-json","-mod=readonly","-timeout=90s","./..."]);throw Error("Empty implementation passed");}
 catch(error){if(error.code!==1||!error.stdout)throw error;output=error.stdout;}
 const events=output.split("\n").filter(Boolean).flatMap(line=>{try{return [JSON.parse(line)];}catch{return [];}});
 const failed=events.filter(e=>e.Action==="fail"&&e.Test).map(e=>e.Test);
 if(required.some(n=>!failed.includes(n)))throw Error("Judges did not all reject stub: "+JSON.stringify({required,failed}));
 const final=path.join(temp,"tasks",String(inspected.items.length-1));
 await put(path.join(final,"candidate"),"internal/portsmith/files.go",stub);
 await put(path.join(final,"candidate"),"internal/portsmith/foundation_test.go",'package portsmith\nimport "testing"\nfunc TestNegativeControlBuild(t *testing.T){}\n');
 await put(path.join(final,"candidate"),"cmd/portsmith/main.go",main);
 const verification=await verifyPort(final,false);
 if(verification.status!=="behavior_failed"||!verification.independent)throw Error("Real verifier did not reach behavior gate: "+JSON.stringify(verification));
 await put(material,"readiness/audit.json",JSON.stringify({started,finished:new Date().toISOString(),preparedSnapshots:inspected.items.length,requiredJudgeTests:required.length,probes,judgeCompile:"passed",negativeControl:"empty typed Go implementation",rejectedBy:failed,realVerifier:verification.status,paidModelCalls:0,caveat:"Preparation, dependency compatibility and judge rejection are verified. The Go product is not implemented yet; correct implementations and comprehensive parity remain to be demonstrated by the migration."},null,2)+"\n");
 console.log(`All ${inspected.items.length} snapshots prepared; ${required.length} independent judges compile and reject the empty implementation; actual verifier reached behavior_failed. No model calls.`);
}finally{await rm(temp,{recursive:true,force:true});}
