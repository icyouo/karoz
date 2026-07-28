package main

import (
	"os/exec"
	"strconv"
	"testing"
)

func TestReplayDeltaTokenLedgerCountsAssistantExactlyOnce(t *testing.T) {
	source, err := staticFS.ReadFile("static/js/context-tokens.js")
	if err != nil {
		t.Fatal(err)
	}
	program := `const vm=require('vm'); const box={window:{}}; vm.createContext(box); vm.runInContext(` + strconv.Quote(string(source)) + `, box); const k=box.window.KarozContextTokens; const history=[{role:'user',intent:'ask',body:'persisted user'}]; const partial=k.rehydrateAssistantTurn('partial'); const complete=k.rehydrateAssistantTurn('partial reply'); const replayed=k.rehydrateAssistantTurn('partial reply'); const expected=k.estimateContextTokens(history,complete,''); const replayedTokens=k.estimateContextTokens(history,replayed,''); if(partial.length!==1||partial[0].role!=='assistant'||complete.length!==1||complete[0].body!=='partial reply'||expected!==replayedTokens) throw Error('replay duplicated assistant tokens');`
	output, err := exec.Command("node", "-e", program).CombinedOutput()
	if err != nil {
		t.Fatalf("replay token contract failed: %v\n%s", err, output)
	}
}
