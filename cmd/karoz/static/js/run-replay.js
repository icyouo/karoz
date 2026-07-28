    // Sequence ownership shared by the POST SSE observer and reconnecting
    // EventSource observer. Keeping it isolated makes reset boundaries
    // executable-testable without requiring a DOM.
    globalThis.KarozRunReplay = {
      accept(state, payload) {
        if (!payload || !payload.run_id) return true;
        if (state.activeRunID && state.activeRunID !== payload.run_id) return false;
        state.activeRunID = payload.run_id;
        const seq = Number(payload.seq || 0);
        if (seq && seq <= Number(state.lastRunSeq || 0)) return false;
        if (seq) state.lastRunSeq = seq;
        return true;
      },
      reset(state, floor) {
        // The server defines floor as the last discarded sequence. The first
        // retained event is therefore floor + 1, and must be accepted once.
        state.lastRunSeq = Math.max(0, Number(floor || 0));
      },
      addToolEvent(replay, event) {
        if (!replay || !event) return false;
        const events = replay.toolEvents || (replay.toolEvents = []);
        const seq = Number(event.seq || 0);
        if (seq && events.some(item => Number(item.seq || 0) === seq)) return false;
        if (!seq && events.some(item => item.kind === event.kind && item.callID === event.callID)) return false;
        events.push(event);
        events.sort((left, right) => Number(left.seq || 0) - Number(right.seq || 0));
        return true;
      },
      transientToolEvents(replay, messages) {
        const durable = new Set((messages || []).map(message => String(message.seq || '')).filter(Boolean));
        return ((replay && replay.toolEvents) || []).filter(event => {
          const messageSeq = String(event.messageSeq || '');
          return !messageSeq || !durable.has(messageSeq);
        });
      }
    };
