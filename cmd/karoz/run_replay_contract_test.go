package main

import (
	"os/exec"
	"strconv"
	"testing"
)

func TestRunReplayJSResetFloorAndSequenceDedup(t *testing.T) {
	source, err := staticFS.ReadFile("static/js/run-replay.js")
	if err != nil {
		t.Fatal(err)
	}
	program := `const vm=require('vm'); const box={globalThis:{}}; vm.createContext(box); vm.runInContext(` + strconv.Quote(string(source)) + `, box); const replay=box.globalThis.KarozRunReplay; const state={activeRunID:'run-1',lastRunSeq:4}; replay.reset(state,10); if(state.lastRunSeq!==10) throw Error('floor cursor'); if(!replay.accept(state,{run_id:'run-1',seq:11})) throw Error('first retained event dropped'); if(replay.accept(state,{run_id:'run-1',seq:11})) throw Error('duplicate event accepted'); if(state.lastRunSeq!==11) throw Error('cursor did not advance'); const view={toolEvents:[]}; if(!replay.addToolEvent(view,{kind:'tool_start',seq:12,callID:'c1',messageSeq:7})) throw Error('event not stored'); if(replay.addToolEvent(view,{kind:'tool_start',seq:12,callID:'c1',messageSeq:7})) throw Error('duplicate tool stored'); const visible=replay.transientToolEvents(view,[{seq:7}]); if(visible.length!==0) throw Error('durable tool duplicated'); if(replay.transientToolEvents(view,[{seq:6}]).length!==1) throw Error('history-fetch gap hidden'); if(replay.accept(state,{run_id:'other',seq:12})) throw Error('cross-run accepted');`
	output, err := exec.Command("node", "-e", program).CombinedOutput()
	if err != nil {
		t.Fatalf("run replay JS contract failed: %v\n%s", err, output)
	}
}

func TestScheduledRunVisibilityJSDoesNotDependOnAgentWorkingState(t *testing.T) {
	source, err := staticFS.ReadFile("static/js/agents.js")
	if err != nil {
		t.Fatal(err)
	}
	program := `const vm=require('vm'); const sources=[]; const calls=[];
class FakeEventSource {
  constructor(url) { this.url=url; this.listeners={}; sources.push(this); }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  close() { this.closed=true; }
}
const agent={id:'karoz',state:'idle',status:'ready'};
const box={
  setTimeout, clearTimeout, EventSource:FakeEventSource, window:{EventSource:FakeEventSource},
  state:{project:{id:'p1'},agent,agents:[agent],chatStreaming:false,view:'agent',activeRunID:'',activeRunAgentID:'',lastRunSeq:0},
  agentPollTimer:null, runtimeStateRefreshTimer:null, runtimeEvents:null, runtimeEventsProjectID:'',
  agentRunEvents:null, agentRunEventsKey:'', agentRunSyncInFlight:false, agentRunSyncQueued:false,
  api:async path=>{ calls.push(path); if(path.endsWith('/agents')) return [agent]; if(path.endsWith('/run')) return {active:true,run:{id:'scheduled-1'}}; throw Error('unexpected '+path); },
  renderAgents(){}, renderRuntimeStrip(){}, agentWorking(){ return false; }, scheduleChatRefresh(){},
  currentAgentID(){ return 'karoz'; }, clearActiveRunReplay(){}, clearCurrentContextTurn(){},
  dispatchAgentSSE(){}, refreshActiveAgentChat:async()=>{}, loadResidentRuntimeState:async()=>{},
  maybeAnimateHandoff(){}, CSS:{escape:value=>value}, $(){ return null; }
}; box.globalThis=box; vm.createContext(box); vm.runInContext(` + strconv.Quote(string(source)) + `, box);
Object.assign(box,{renderAgents(){},renderRuntimeStrip(){},agentWorking(){return false;},scheduleChatRefresh(){},currentAgentID(){return 'karoz';},clearActiveRunReplay(){},clearCurrentContextTurn(){},dispatchAgentSSE(){},refreshActiveAgentChat:async()=>{},loadResidentRuntimeState:async()=>{},maybeAnimateHandoff(){}});
const tick=()=>new Promise(resolve=>setTimeout(resolve,0));
(async()=>{
  await box.refreshAgentStates(); await tick();
  if(!calls.some(path=>path.endsWith('/agents/karoz/run'))) throw Error('idle poll did not probe active Run');
  if(!sources.some(source=>source.url.includes('/runs/scheduled-1/events?after=0'))) throw Error('idle poll did not attach replay stream');
  box.syncRuntimeEvents(); const runtime=sources.find(source=>source.url.endsWith('/runtime-events'));
  if(!runtime) throw Error('runtime subscription missing');
  box.agentRunEvents=null; box.agentRunEventsKey=''; box.state.activeRunID=''; box.state.activeRunAgentID='';
  const sourcesBeforeRuntime=sources.length;
  calls.length=0;
  runtime.listeners.runtime[0]({data:JSON.stringify({agents:[agent],event:{kind:'agent_run_changed'}})});
  await tick();
  if(!calls.some(path=>path.endsWith('/agents/karoz/run'))) throw Error('runtime event did not probe active Run');
  if(sources.length!==sourcesBeforeRuntime+1 || !sources[sources.length-1].url.includes('/runs/scheduled-1/events?after=0')) throw Error('runtime event did not attach replay stream');
})().catch(error=>{ console.error(error); process.exitCode=1; });`
	output, err := exec.Command("node", "-e", program).CombinedOutput()
	if err != nil {
		t.Fatalf("scheduled Run visibility JS contract failed: %v\n%s", err, output)
	}
}
