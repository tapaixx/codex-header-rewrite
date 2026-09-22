// Contrast, hit-area, cursor, transition and focus-ring audit over the
// preview fixtures, in every workspace and both themes. Reads computed
// styles, resolves each text node's effective background through its
// ancestors, and fails on any ratio under WCAG AA (4.5:1, 3:1 for large
// text). Disabled controls are skipped: WCAG exempts inactive components.
// Run from the repo root with CHROMIUM_PATH set, after make-preview.mjs.
const { chromium } = await import('playwright');
const b = await chromium.launch({headless:true,executablePath:process.env.CHROMIUM_PATH,args:['--no-sandbox']});
const p = await b.newPage();
const errs=[]; p.on('pageerror',e=>errs.push(e.message)); p.on('console',m=>{ if(m.type()==='error') errs.push('console: '+m.text()); });
await p.setViewportSize({width:1440,height:1000});
await p.goto(new URL('../preview.html', import.meta.url).href);
await p.waitForSelector('#rulePanel');
const probe = () => {
  const cv = document.createElement('canvas').getContext('2d');
  const norm = (s) => { const cm=/^color\(srgb ([\d.]+) ([\d.]+) ([\d.]+)(?: \/ ([\d.]+))?\)/.exec(s); if(cm) return [cm[1]*255,cm[2]*255,cm[3]*255,cm[4]===undefined?1:+cm[4]]; cv.fillStyle = '#000'; cv.fillStyle = s; const v = cv.fillStyle; if (v[0]==='#') return [parseInt(v.slice(1,3),16),parseInt(v.slice(3,5),16),parseInt(v.slice(5,7),16),1]; const m = v.match(/rgba?\(([^)]+)\)/); if(!m) return [0,0,0,1]; const a = m[1].split(',').map(parseFloat); return [a[0],a[1],a[2],a.length>3?a[3]:1]; };
  const lum = ([r,g,bl]) => { const f=c=>{c/=255;return c<=.03928?c/12.92:Math.pow((c+.055)/1.055,2.4);}; return .2126*f(r)+.7152*f(g)+.0722*f(bl); };
  const ratio = (a,b) => { const l1=lum(a),l2=lum(b); return (Math.max(l1,l2)+.05)/(Math.min(l1,l2)+.05); };
  const over = (top, under) => { const a=top[3]; return [top[0]*a+under[0]*(1-a), top[1]*a+under[1]*(1-a), top[2]*a+under[2]*(1-a), 1]; };
  const bgOf = (el) => { const layers=[]; let e=el; let opacity=1; while(e && e!==document.documentElement){ const cs=getComputedStyle(e); opacity*=parseFloat(cs.opacity||1); const c=norm(cs.backgroundColor); if(c[3]>=1){ let out=c; for(let i=layers.length-1;i>=0;i--) out=over(layers[i],out); return {bg:out,opacity}; } if(c[3]>0) layers.push(c); e=e.parentElement; } let out=norm(getComputedStyle(document.documentElement).backgroundColor); if(out[3]<1) out=[255,255,255,1]; for(let i=layers.length-1;i>=0;i--) out=over(layers[i],out); return {bg:out,opacity}; };
  const sel = (el) => { let s=el.tagName.toLowerCase(); if(el.id) s+='#'+el.id; else if(el.className&&typeof el.className==='string') s+='.'+el.className.trim().split(/\s+/).slice(0,2).join('.'); const par=el.parentElement; if(par && par!==document.body){ let ps=par.tagName.toLowerCase(); if(par.id) ps+='#'+par.id; else if(par.className&&typeof par.className==='string') ps+='.'+par.className.trim().split(/\s+/)[0]; s=ps+' > '+s; } return s; };
  const visible = (el) => { if(!el.getClientRects().length) return false; const r=el.getBoundingClientRect(); if(r.width<1||r.height<1) return false; const cs=getComputedStyle(el); return cs.visibility!=='hidden'; };
  const contrast=[]; const seen=new Set();
  for (const el of document.querySelectorAll('body *')) {
    if(!visible(el)) continue;
    if(el.closest('[data-disabled="1"], :disabled') || [...el.closest('label')?.querySelectorAll(':disabled')||[]].length) continue;
    const txt=[...el.childNodes].filter(n=>n.nodeType===3).map(n=>n.textContent.trim()).join(' ').trim();
    if(!txt) continue;
    const cs=getComputedStyle(el); const fg0=norm(cs.color); const {bg,opacity}=bgOf(el);
    let fg = fg0[3]<1 ? over(fg0,bg) : fg0; if(opacity<1) fg=over([fg[0],fg[1],fg[2],opacity],bg);
    const r=ratio(fg,bg); const fs=parseFloat(cs.fontSize); const bold=parseInt(cs.fontWeight)>=700; const large = fs>=24 || (fs>=18.66&&bold);
    const need = large?3:4.5;
    if (r<need) { const key=sel(el)+'|'+cs.color+'|'+r.toFixed(2); if(seen.has(key)) continue; seen.add(key); contrast.push(`${r.toFixed(2)}:1 (need ${need}) ${sel(el)} "${txt.slice(0,28)}" fg=${cs.color} bg=rgb(${bg.slice(0,3).map(Math.round)}) ${fs}px/${cs.fontWeight}`); }
  }
  const radii=[]; const rseen=new Set();
  for (const el of document.querySelectorAll('body *')) {
    if(!visible(el)) continue; const cs=getComputedStyle(el); const r=parseFloat(cs.borderTopLeftRadius); if(!r) continue;
    const hasSurface = norm(cs.backgroundColor)[3]>0 || (parseFloat(cs.borderTopWidth)>0 && norm(cs.borderTopColor)[3]>0) || cs.boxShadow!=='none' || cs.outlineStyle!=='none';
    if(!hasSurface) continue;
    let par=el.parentElement; while(par && par!==document.body){ const pcs=getComputedStyle(par); const pr=parseFloat(pcs.borderTopLeftRadius); const psurf = norm(pcs.backgroundColor)[3]>0 || (parseFloat(pcs.borderTopWidth)>0&&norm(pcs.borderTopColor)[3]>0); if(pr&&psurf){ const pad=parseFloat(pcs.paddingLeft)+parseFloat(pcs.borderLeftWidth); const er=el.getBoundingClientRect(), pr2=par.getBoundingClientRect(); const inset=Math.min(er.left-pr2.left, er.top-pr2.top, pr2.right-er.right, pr2.bottom-er.bottom); if(inset<=12){ const expect=Math.max(0,pr-inset); const key=sel(el)+'|'+r+'|'+pr; if(!rseen.has(key)&&Math.abs(expect-r)>1.5){ rseen.add(key); radii.push(`${sel(el)} r=${r} inside ${sel(par)} r=${pr} inset=${inset.toFixed(0)} → expect ≈${expect.toFixed(0)}`);} } break; } par=par.parentElement; }
  }
  const trans=[...document.querySelectorAll('body *')].filter(el=>getComputedStyle(el).transitionProperty==='all' && getComputedStyle(el).transitionDuration!=='0s').map(sel).filter((v,i,a)=>a.indexOf(v)===i);
  const inter=[...document.querySelectorAll('button,a[href],input,select,textarea,[role=button],[tabindex]:not([tabindex="-1"]),summary,label.switch')].filter(visible);
  const small=[]; const nocursor=[]; const sseen=new Set();
  for(const el of inter){ const r=el.getBoundingClientRect(); const cs=getComputedStyle(el); let w=r.width,h=r.height; const after=getComputedStyle(el,'::after'); if(after.content!=='none'&&after.position==='absolute'){ w=Math.max(w,parseFloat(after.width)||0); h=Math.max(h,parseFloat(after.height)||0);} const k=sel(el); if(sseen.has(k)) continue; sseen.add(k); if((w<24||h<24) && !el.matches('input[type=radio],input[type=checkbox]')) small.push(`${k} ${Math.round(w)}x${Math.round(h)}`); if(!el.disabled && ['button','a','summary','label'].includes(el.tagName.toLowerCase()) && !['pointer','help','text'].includes(cs.cursor)) nocursor.push(`${k} cursor=${cs.cursor}`); }
  const selects=[...document.querySelectorAll('select')].filter(visible).map(el=>{const cs=getComputedStyle(el);return `${sel(el)} bg=${cs.backgroundColor} color=${cs.color}`;});
  const fonts=new Map(); for(const el of document.querySelectorAll('body *')){ if(!visible(el)) continue; const txt=[...el.childNodes].some(n=>n.nodeType===3&&n.textContent.trim()); if(!txt) continue; const fs=getComputedStyle(el).fontSize; fonts.set(fs,(fonts.get(fs)||0)+1);} 
  return {contrast, radii, trans, small, nocursor, selects, colorScheme:getComputedStyle(document.documentElement).colorScheme, fonts:[...fonts.entries()].sort((a,b)=>parseFloat(a[0])-parseFloat(b[0])).map(([k,v])=>`${k}×${v}`).join(' ')};
};
const out={};
for (const theme of ['white','dark']) {
  await p.evaluate(t=>{document.documentElement.dataset.theme=t;},theme);
  for (const ws of ['rules','history','decode']) {
    await p.evaluate(v=>setWorkspace(v,false),ws); await p.waitForTimeout(150);
    if (ws==='history') { const row=await p.$('#historyPanel tbody tr'); if(row){ await row.click(); await p.waitForTimeout(250);}  }
    out[`${theme}/${ws}`]=await p.evaluate(probe);
    if (ws==='history') { await p.keyboard.press('Escape'); await p.waitForTimeout(150); }
  }
}
// focus walk (light, rules)
await p.evaluate(t=>{document.documentElement.dataset.theme=t;},'white'); await p.evaluate(v=>setWorkspace(v,false),'rules'); await p.waitForTimeout(120);
await p.keyboard.press('Tab');
const focus=[]; for(let i=0;i<45;i++){ const info=await p.evaluate(()=>{const el=document.activeElement; if(!el||el===document.body) return null; const cs=getComputedStyle(el); const ring = (cs.outlineStyle!=='none'&&parseFloat(cs.outlineWidth)>0) || cs.boxShadow!=='none'; let s=el.tagName.toLowerCase()+(el.id?'#'+el.id:'')+(el.className&&typeof el.className==='string'?'.'+el.className.trim().split(/\s+/)[0]:''); return `${s} ring=${ring?'yes':'NO'} outline=${cs.outlineStyle}/${cs.outlineWidth}`;}); if(info) focus.push(info); await p.keyboard.press('Tab'); }
const noRing = focus.filter(f=>f.includes('ring=NO'));
for (const [k,v] of Object.entries(out)) {
  console.log(`\n===== ${k} =====`);
  console.log(`fonts: ${v.fonts}`);
  console.log(`color-scheme: ${v.colorScheme}`);
  console.log(`contrast fails (${v.contrast.length}):`); v.contrast.slice(0,40).forEach(l=>console.log('  '+l));
  console.log(`radius mismatches (${v.radii.length}):`); v.radii.slice(0,30).forEach(l=>console.log('  '+l));
  console.log(`transition:all (${v.trans.length}): ${v.trans.slice(0,10).join(', ')}`);
  console.log(`small targets (${v.small.length}):`); v.small.slice(0,25).forEach(l=>console.log('  '+l));
  console.log(`no pointer cursor (${v.nocursor.length}):`); v.nocursor.slice(0,20).forEach(l=>console.log('  '+l));
  if (v.selects.length) console.log(`selects: ${v.selects.join(' | ')}`);
}
console.log(`\n===== focus walk (${focus.length} stops, ${noRing.length} without ring) =====`); noRing.forEach(l=>console.log('  '+l));
console.log('\nerrors:', errs.length?errs:'none');
await b.close();
const total=Object.values(out).reduce((n,v)=>n+v.contrast.length+v.small.length+v.trans.length+v.nocursor.length,0)+noRing.length+errs.length;
console.log(total?`contrast audit: ${total} finding(s)`:'contrast audit: passed');
process.exit(total?1:0);
