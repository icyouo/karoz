    function appendAgentMessage(role, text, meta = '') {
      const empty = $('agentOutput').querySelector('.chat-empty');
      if (empty) empty.remove();
      const item = document.createElement('div');
      const isAgent = role !== 'you';
      item.className = 'chat-message ' + (isAgent ? 'assistant' : 'user');
      const metaText = meta ? ' · ' + meta : '';
      item.innerHTML = '<div class="chat-body"><div class="chat-meta"><span class="chat-name">' + (isAgent ? escapeHTML(currentAgentLabel()) : 'you') + '</span><span>' + (isAgent ? escapeHTML((state.agent && state.agent.role) || 'resident') : 'input') + metaText + '</span></div><div class="chat-bubble"></div></div>';
      setBubbleContent(item.querySelector('.chat-bubble'), text, isAgent);
      $('agentOutput').appendChild(item);
      $('agentOutput').scrollTop = $('agentOutput').scrollHeight;
      return item;
    }
    function appendToolMessage(role, tool, callID, content, success = true) {
      const empty = $('agentOutput').querySelector('.chat-empty');
      if (empty) empty.remove();
      const item = document.createElement('div');
      const isResult = role === 'tool_result';
      item.className = 'tool-group ' + (isResult ? (success ? 'success' : 'failed') : '');
      const summary = toolSummary(role, tool, content, success);
      item.innerHTML = '<button type="button" class="tool-toggle"><div class="tool-head"><span class="tool-chevron">▸</span><strong>' + escapeHTML(formatToolName(tool)) + '</strong><span>' + escapeHTML(isResult ? 'result' : 'call') + (callID ? ' · ' + escapeHTML(callID) : '') + '</span><span class="tool-summary">' + escapeHTML(summary) + '</span></div></button><div class="tool-body"></div>';
      item.querySelector('.tool-body').textContent = compactToolContent(content);
      item.querySelector('.tool-toggle').onclick = () => item.classList.toggle('expanded');
      $('agentOutput').appendChild(item);
      $('agentOutput').scrollTop = $('agentOutput').scrollHeight;
      return item;
    }
    function parseChoiceRequestResult(content) {
      try {
        const payload = typeof content === 'string' ? JSON.parse(content || '{}') : content;
        if (!payload || payload.kind !== 'choice_request' || !Array.isArray(payload.choices)) return null;
        return payload;
      } catch {
        return null;
      }
    }
    function appendChoiceRequestFromResult(content, interactive = true) {
      const choice = parseChoiceRequestResult(content);
      if (!choice) return false;
      appendChoiceRequest(choice, interactive);
      return true;
    }
    function appendChoiceRequest(choice, interactive = true) {
      const empty = $('agentOutput').querySelector('.chat-empty');
      if (empty) empty.remove();
      const item = document.createElement('div');
      item.className = 'choice-card';
      const question = document.createElement('div');
      question.className = 'choice-question';
      question.textContent = choice.question || 'Choose an option';
      const options = document.createElement('div');
      options.className = 'choice-options';
      (choice.choices || []).forEach((option, index) => {
        const button = document.createElement('button');
        button.type = 'button';
        button.className = 'choice-option';
        button.disabled = !interactive;
        const marker = choice.mode === 'yes_no' ? '' : String(index + 1);
        button.innerHTML = '<span class="choice-index">' + escapeHTML(marker) + '</span><span><strong class="choice-label">' + escapeHTML(option.label || option.id || ('Option ' + (index + 1))) + '</strong>' + (option.description ? '<span class="choice-desc">' + escapeHTML(option.description) + '</span>' : '') + '</span>';
        button.onclick = async () => {
          if (!interactive || item.classList.contains('answered')) return;
          item.classList.add('answered');
          button.classList.add('selected');
          options.querySelectorAll('button').forEach(candidate => { candidate.disabled = true; });
          const id = option.id || String(index + 1);
          const label = option.label || id;
          $('agentStatus').textContent = currentAgentLabel() + ' · submitting choice ·';
          for (let attempt = 0; attempt < 50 && currentAgentWorking(); attempt++) {
            await new Promise(resolve => setTimeout(resolve, 100));
            await refreshAgentStates();
          }
          if (currentAgentWorking()) {
            item.classList.remove('answered');
            button.classList.remove('selected');
            options.querySelectorAll('button').forEach(candidate => { candidate.disabled = false; });
            notify('The current agent turn is still finishing. Try the choice again.', 'error');
            return;
          }
          await sendAgentMessage('Selected: ' + label, id, []);
        };
        options.appendChild(button);
      });
      item.appendChild(question);
      item.appendChild(options);
      $('agentOutput').appendChild(item);
      $('agentOutput').scrollTop = $('agentOutput').scrollHeight;
      return item;
    }
    function formatToolName(tool) {
      return String(tool || 'tool').replace(/_/g, ' ');
    }
    function compactToolContent(content) {
      const text = typeof content === 'string' ? content : JSON.stringify(content || {});
      if (text.length <= 3000) return text;
      return text.slice(0, 3000) + '\n...[truncated]';
    }
    function toolSummary(role, tool, content, success) {
      const text = typeof content === 'string' ? content : JSON.stringify(content || {});
      if (tool === 'bash') {
        try {
          const parsed = JSON.parse(text || '{}');
          if (role === 'tool_call') return parsed.command || 'bash';
          const code = parsed.code !== undefined ? 'exit ' + parsed.code : (success ? 'ok' : 'failed');
          const output = parsed.stdout || parsed.stderr || parsed.error || '';
          return code + (output ? ' · ' + String(output).replace(/\s+/g, ' ').slice(0, 160) : '');
        } catch {
          return text.replace(/\s+/g, ' ').slice(0, 180);
        }
      }
      if (role === 'tool_result') return (success ? 'completed' : 'failed') + ' · ' + text.replace(/\s+/g, ' ').slice(0, 160);
      return text.replace(/\s+/g, ' ').slice(0, 180);
    }
    function setBubbleContent(el, text, markdown) {
      if (markdown) {
        el.innerHTML = renderMarkdown(text || '');
      } else {
        el.textContent = text || '';
      }
    }
    function escapeHTML(value) {
      return String(value || '').replace(/[&<>"']/g, ch => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[ch]));
    }
    function renderMarkdown(value) {
      const lines = String(value || '').replace(/\r\n/g, '\n').split('\n');
      const out = [];
      let paragraph = [];
      let listType = '';
      let inCode = false;
      let codeLines = [];
      const closeParagraph = () => {
        if (!paragraph.length) return;
        out.push('<p>' + paragraph.map(renderInlineMarkdown).join('<br>') + '</p>');
        paragraph = [];
      };
      const closeList = () => {
        if (!listType) return;
        out.push('</' + listType + '>');
        listType = '';
      };
      const closeBlocks = () => {
        closeParagraph();
        closeList();
      };
      const openList = (type) => {
        closeParagraph();
        if (listType && listType !== type) closeList();
        if (!listType) {
          listType = type;
          out.push('<' + type + '>');
        }
      };
      const flushCode = () => {
        out.push('<pre class="md-code"><code>' + escapeHTML(codeLines.join('\n').replace(/^\n|\n$/g, '')) + '</code></pre>');
        codeLines = [];
      };
      for (let i = 0; i < lines.length; i++) {
        const raw = lines[i];
        const trimmed = raw.trim();
        if (/^```/.test(trimmed)) {
          if (inCode) {
            flushCode();
            inCode = false;
          } else {
            closeBlocks();
            inCode = true;
            codeLines = [];
          }
          continue;
        }
        if (inCode) {
          codeLines.push(raw);
          continue;
        }
        if (!trimmed) {
          closeBlocks();
          continue;
        }
        if (i + 2 < lines.length && /^\s*\|.+\|\s*$/.test(raw) && /^\s*\|[\s:|\-]+\|\s*$/.test(lines[i + 1])) {
          const tableLines = [raw, lines[i + 1]];
          i += 2;
          while (i < lines.length && /^\s*\|.*\|\s*$/.test(lines[i])) {
            tableLines.push(lines[i]);
            i++;
          }
          i--;
          closeBlocks();
          out.push(renderMarkdownTableLines(tableLines));
          continue;
        }
        if (/^(-{3,}|\*{3,}|_{3,})$/.test(trimmed)) {
          closeBlocks();
          out.push('<hr>');
          continue;
        }
        const heading = raw.match(/^\s{0,3}(#{1,6})\s+(.+?)\s*#*\s*$/);
        if (heading) {
          closeBlocks();
          const level = Math.min(6, heading[1].length);
          out.push('<h' + level + '>' + renderInlineMarkdown(heading[2]) + '</h' + level + '>');
          continue;
        }
        const quote = raw.match(/^\s{0,3}>\s?(.*)$/);
        if (quote) {
          closeBlocks();
          const quoteLines = [quote[1]];
          while (i + 1 < lines.length) {
            const next = lines[i + 1].match(/^\s{0,3}>\s?(.*)$/);
            if (!next) break;
            quoteLines.push(next[1]);
            i++;
          }
          out.push('<blockquote>' + quoteLines.map(renderInlineMarkdown).join('<br>') + '</blockquote>');
          continue;
        }
        const unordered = raw.match(/^(\s*)[-*+]\s+(.+)$/);
        if (unordered) {
          openList('ul');
          out.push('<li>' + renderInlineMarkdown(unordered[2].replace(/^#{1,6}\s+/, '')) + '</li>');
          continue;
        }
        const ordered = raw.match(/^(\s*)\d+[.)]\s+(.+)$/);
        if (ordered) {
          openList('ol');
          out.push('<li>' + renderInlineMarkdown(ordered[2].replace(/^#{1,6}\s+/, '')) + '</li>');
          continue;
        }
        closeList();
        paragraph.push(raw);
      }
      if (inCode) flushCode();
      closeBlocks();
      return out.join('');
    }
    function renderInlineMarkdown(value) {
      const codes = [];
      let text = String(value || '').replace(/`([^`]+)`/g, (_, code) => {
        const token = '\u0000INLINE' + codes.length + '\u0000';
        codes.push('<code class="md-inline">' + escapeHTML(code) + '</code>');
        return token;
      });
      text = escapeHTML(text);
      text = text.replace(/\*\*([\s\S]+?)\*\*/g, '<strong>$1</strong>');
      text = text.replace(/__([\s\S]+?)__/g, '<strong>$1</strong>');
      text = text.replace(/~~([\s\S]+?)~~/g, '<del>$1</del>');
      text = text.replace(/(^|[^\*])\*([^*\n]+)\*/g, '$1<em>$2</em>');
      text = text.replace(/\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)/g, '<a href="$2" target="_blank" rel="noreferrer">$1</a>');
      text = text.replace(/(^|[\s(])(https?:\/\/[^\s<)]+)/g, '$1<a href="$2" target="_blank" rel="noreferrer">$2</a>');
      codes.forEach((html, index) => {
        text = text.replace('\u0000INLINE' + index + '\u0000', html);
      });
      return text;
    }
    function renderMarkdownTables(text) {
      return text.replace(/((?:^|\n)\|.+\|\n\|[\s:|\\-]+\|\n(?:\|.*\|\n?)+)/g, (table) => {
        const rows = table.trim().split('\n').filter(Boolean);
        if (rows.length < 3) return table;
        const cells = (row) => row.replace(/^\||\|$/g, '').split('|').map(cell => cell.trim());
        const head = cells(rows[0]);
        const body = rows.slice(2).map(cells);
        return '<table><thead><tr>' + head.map(cell => '<th>' + cell + '</th>').join('') + '</tr></thead><tbody>' + body.map(row => '<tr>' + row.map(cell => '<td>' + cell + '</td>').join('') + '</tr>').join('') + '</tbody></table>';
      });
    }
    function renderMarkdownTableLines(rows) {
      if (rows.length < 3) return '<p>' + rows.map(renderInlineMarkdown).join('<br>') + '</p>';
      const cells = (row) => row.replace(/^\s*\||\|\s*$/g, '').split('|').map(cell => cell.trim());
      const head = cells(rows[0]);
      const body = rows.slice(2).map(cells);
      return '<table><thead><tr>' + head.map(cell => '<th>' + renderInlineMarkdown(cell) + '</th>').join('') + '</tr></thead><tbody>' + body.map(row => '<tr>' + row.map(cell => '<td>' + renderInlineMarkdown(cell) + '</td>').join('') + '</tr>').join('') + '</tbody></table>';
    }
