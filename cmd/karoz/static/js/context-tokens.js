    (function(global) {
      const MAX_TRANSCRIPT_ITEMS = 50;
      const MAX_TRANSCRIPT_CHARS = 24000;
      function contextRecord(message) {
        return [message && message.role, message && message.intent, message && message.body]
          .filter(Boolean)
          .join('\n');
      }
      function compactMessages(history, currentTurn) {
        const items = (history || []).concat(currentTurn || []);
        const reversed = [];
        let used = 0;
        for (let index = items.length - 1; index >= 0; index -= 1) {
          const record = contextRecord(items[index]);
          if (!record.trim()) continue;
          const cost = Array.from(record).length;
          if (reversed.length && used + cost > MAX_TRANSCRIPT_CHARS) break;
          reversed.push(items[index]);
          used += cost;
          if (reversed.length >= MAX_TRANSCRIPT_ITEMS) break;
        }
        return reversed.reverse();
      }
      function estimateTextTokens(text) {
        const characters = Array.from(String(text || ''));
        if (!characters.length) return 0;
        const cjk = characters.filter(character => /[\u3040-\u30ff\u3400-\u9fff\uf900-\ufaff]/.test(character)).length;
        return Math.ceil((characters.length - cjk) / 4 + cjk * 1.5);
      }
      function estimateContextTokens(history, currentTurn, draft) {
        const content = compactMessages(history, currentTurn)
          .map(contextRecord)
          .concat(String(draft || ''))
          .join('\n');
        if (!content.trim()) return 0;
        return estimateTextTokens(content);
      }
      function beginCurrentTurn(message) {
        return [
          { role: 'user', intent: 'message', body: String(message || '') },
          { role: 'assistant', intent: 'response', body: '' },
        ];
      }
      function updateAssistantTurn(currentTurn, body) {
        const turn = (currentTurn || []).slice();
        const index = turn.findIndex(message => message.role === 'assistant');
        if (index >= 0) turn[index] = { ...turn[index], body: String(body || '') };
        return turn;
      }
      // Reconnect replay has durable user/tool history already. Rehydrate only
      // the in-flight assistant text, replacing rather than appending it so a
      // replayed sequence cannot inflate the current-turn token estimate.
      function rehydrateAssistantTurn(body) {
        return [{ role: 'assistant', intent: 'response', body: String(body || '') }];
      }
      function appendTurnEvent(currentTurn, role, intent, body) {
        return (currentTurn || []).concat({ role, intent, body: typeof body === 'string' ? body : JSON.stringify(body || {}) });
      }
      global.KarozContextTokens = {
        appendTurnEvent,
        beginCurrentTurn,
        compactMessages,
        estimateContextTokens,
        rehydrateAssistantTurn,
        updateAssistantTurn,
      };
    })(window);
