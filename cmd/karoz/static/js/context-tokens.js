    (function(global) {
      function compactMessages(history, currentTurn) {
        return (history || []).concat(currentTurn || []).slice(-50);
      }
      function estimateTextTokens(text) {
        const characters = Array.from(String(text || ''));
        if (!characters.length) return 0;
        const cjk = characters.filter(character => /[\u3040-\u30ff\u3400-\u9fff\uf900-\ufaff]/.test(character)).length;
        return Math.ceil((characters.length - cjk) / 4 + cjk * 1.5);
      }
      function estimateContextTokens(history, currentTurn, draft) {
        const content = compactMessages(history, currentTurn)
          .map(message => [message.role, message.intent, message.body].filter(Boolean).join('\n'))
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
      function appendTurnEvent(currentTurn, role, intent, body) {
        return (currentTurn || []).concat({ role, intent, body: typeof body === 'string' ? body : JSON.stringify(body || {}) });
      }
      global.KarozContextTokens = {
        appendTurnEvent,
        beginCurrentTurn,
        compactMessages,
        estimateContextTokens,
        updateAssistantTurn,
      };
    })(window);
