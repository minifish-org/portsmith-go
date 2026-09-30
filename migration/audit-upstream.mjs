// Positive control: the workflow fixture used by the Go judges must also run
// through the original TS executor. No provider or paid model is involved.
import { execFileSync } from "node:child_process";
import { mkdtemp, mkdir, writeFile, readFile, rm } from "node:fs/promises";
import { tmpdir } from "node:os";
import path from "node:path";
import { createHash } from "node:crypto";
import { pathToFileURL } from "node:url";
import { executor, material, toolEnvironment } from "./hydrate.mjs";
const {analyze}=await import(pathToFileURL(path.join(executor,"dist/analyze.js")));
const {migrate}=await import(pathToFileURL(path.join(executor,"dist/migrate.js")));
const {writeCandidate}=await import(pathToFileURL(path.join(executor,"dist/workspace.js")));
Object.assign(process.env,toolEnvironment());
const hash=b=>createHash("sha256").update(b).digest("hex");
const put=async(root,n,b)=>{const p=path.join(root,n);await mkdir(path.dirname(p),{recursive:true});await writeFile(p,b);};
const json=(root,n,v)=>put(root,n,JSON.stringify(v));
const temp=await mkdtemp(path.join(tmpdir(),"portsmith-native-workflow-positive-"));
const results=[];
try{
 for(const mode of ["v1","v2","additive"]){
  const version=mode==="v1"?1:2;const source=path.join(temp,mode,"source"),project=path.join(temp,mode,"target"),plan=path.join(project,"migration");
  const git=(...args)=>execFileSync("git",args,{cwd:project,encoding:"utf8"}).trim();
  await put(source,"LICENSE","fixture license");await put(source,"value.ts","export const Value=42;\n");
  await put(project,".gitignore",".portsmith/\n");await put(project,"go.mod","module example.com/candidate\n\ngo 1.24\n");
  git("init","-q");git("config","user.name","Portsmith Judge");git("config","user.email","judge@example.invalid");git("config","commit.gpgsign","false");git("add",".");git("commit","-qm","Initial fixture");
  await put(plan,"go.mod","module example.com/fixture-material\n\ngo 1.24\n");await put(plan,"RULEBOOK.md","Preserve behavior.\n");await put(plan,"contract.md","Implement Value() int returning 42; write a self-test.\n");
  await put(plan,"judges/port/value_judge_test.go",'package port\nimport "testing"\nfunc TestPortsmithJudgeValue(t *testing.T){if Value()!=42{t.Fatal("wrong value")}}\n');
  const analysis=JSON.stringify(await analyze(source));await put(plan,"analysis.json",analysis);
  const outputs=["port/value.go","port/value_test.go"],spec={outputs,tests:["TestPortsmithJudgeValue"],judge:"judges",contract:"contract.md"};
  const p={version,source,revision:"fixture",analysisSha256:hash(analysis)};
  const w={version,project:"..",runs:".portsmith/runs",bootstrap:["migration"]};
  if(version===1){p.units=[{id:"value",goal:"Port Value",targetPackage:"port",files:["value.ts"],references:[],dependsOn:[],acceptance:["Value returns 42"],notes:[]}];p.packageCycles=[];w.units={value:spec};}
  else{p.modules=[{id:"app",dependsOn:[],batches:["runtime"]}];p.batches=[{id:"runtime",module:"app",dependsOn:[],sources:["value.ts"],references:[],outputs,behaviors:["value"],acceptance:["42"]}];Object.assign(spec,{id:"port",sources:["value.ts"],goal:"Port Value"});w.startPolicy="all-prepared";w.batches={runtime:{status:"ready",steps:[spec]}};}
  await json(plan,"plan.json",p);await json(plan,"workflow.json",w);
  let implementation='package port\nfunc Value() int {return 42}\n';
  if(mode==="additive"){
   const base='package base\nfunc Number() int {return 42}\n';await put(project,"internal/base/value.go",base);git("add",".");git("commit","-qm","Frozen baseline");
   w.journal=".portsmith/additive/modules.json";w.baseline={commit:git("rev-parse","HEAD"),files:[{name:"internal/base/value.go",sha256:hash(base)}]};await json(plan,"workflow.json",w);
   implementation='package port\nimport "example.com/candidate/internal/base"\nfunc Value() int {return base.Number()}\n';
  }
  let calls=0;
  const options={plan,commit:true,maxAttempts:1,generate:async(root,feedback)=>{
   calls++;await writeCandidate(root,"port/value.go",mode==="v2"&&calls===1?'package port\nfunc Value() int {return 41}\n':implementation);
   await writeCandidate(root,"port/value_test.go",'package port\nimport "testing"\nfunc TestCandidate(t *testing.T){if Value()<0{t.Fatal("negative")}}\n');return {status:"candidate_ready"};
  }};
  const checked=await migrate({...options,check:true});if(checked.status!=="ready"||calls!==0)throw Error("Invalid preflight fixture");
  let result;
  try{result=await migrate(options);}catch(error){if(mode!=="v2"||!String(error.message).includes("reached the limit"))throw error;result={status:"repair-budget"};}
  if(mode==="v2"){if(result.status==="complete")throw Error("Bad candidate accepted");result=await migrate(options);}
  if(result.status!=="complete")throw Error("Fixture cannot finish: "+JSON.stringify(result));
  const before=calls,head=git("rev-parse","HEAD");await migrate(options);if(calls!==before||git("rev-parse","HEAD")!==head||git("status","--porcelain")!=="")throw Error("Non-idempotent fixture resume");
  results.push({mode,status:"passed",generatorCalls:calls});console.log(`Upstream ${mode} positive control passed.`);
 }
 await put(material,"readiness/upstream.json",JSON.stringify({at:new Date().toISOString(),results,paidModelCalls:0},null,2)+"\n");
}finally{await rm(temp,{recursive:true,force:true});}
