// Проверка SVG, измерение текста настоящим шрифтом и снимки каждого макета.
// Обзорный лист собирается тем же браузером: внешние библиотеки растра не нужны.
// LIDRADAR_SHEETS — регулярное выражение по идентификаторам листов, снимки которых
// нужно обновить (остальные PNG сохраняются); без него обновляются все.
import fs from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import {fileURLToPath,pathToFileURL} from 'node:url';
const root=path.resolve(path.dirname(fileURLToPath(import.meta.url)),'..');
const modules=process.env.LIDRADAR_NODE_MODULES||path.join(root,'node_modules');
const {chromium}=await import(pathToFileURL(path.join(modules,'playwright','index.mjs')));
const manifest=JSON.parse(await fs.readFile(path.join(root,'manifest.json'),'utf8'));
const only=process.env.LIDRADAR_SHEETS?new RegExp(process.env.LIDRADAR_SHEETS):null;
const chrome='/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
const useChrome=await fs.access(chrome).then(()=>true,()=>false);
const browser=await chromium.launch({headless:true,...(useChrome?{executablePath:chrome}:{}),args:['--allow-file-access-from-files']});
const page=await browser.newPage({viewport:{width:1536,height:1200},deviceScaleFactor:1});
await page.goto(pathToFileURL(path.join(root,'index.html')).href);
await page.evaluate(()=>document.fonts.ready);
const results=[];
for(const s of manifest){
  const raw=await fs.readFile(path.join(root,s.file),'utf8');
  const xml=await page.evaluate(raw=>{const d=new DOMParser().parseFromString(raw,'image/svg+xml');const ids=[...d.querySelectorAll('[id]')].map(n=>n.id);return {valid:!d.querySelector('parsererror'),duplicateIds:ids.filter((v,i)=>ids.indexOf(v)!==i)};},raw);
  const target=page.locator(`section[id="${s.id}"] > svg`);
  const result=await target.evaluate(svg=>{
    const errors=[];const w=svg.viewBox.baseVal.width,h=svg.viewBox.baseVal.height;
    for(const n of svg.querySelectorAll('text')){const b=n.getBBox(),limit=+n.getAttribute('data-max-width');
      if(limit&&b.width>limit+1)errors.push({text:n.textContent,reason:'шире выделенной области',width:Math.round(b.width),limit});
      if(b.x<-.5||b.y<-.5||b.x+b.width>w+.5||b.y+b.height>h+.5)errors.push({text:n.textContent,reason:'за пределами макета',bounds:{x:b.x,y:b.y,w:b.width,h:b.height}});
      if(getComputedStyle(n).fontFamily!=='Inter')errors.push({text:n.textContent,reason:'неверный шрифт'});
    }
    return {width:w,height:h,textLayers:svg.querySelectorAll('text').length,rasterImages:svg.querySelectorAll('image').length,externalReferences:svg.querySelectorAll('[href],[xlink\\:href]').length,fontLoaded:document.fonts.check('14px Inter'),errors};
  });
  if(!xml.valid||xml.duplicateIds.length)result.errors.push({reason:'некорректный XML или повтор идентификатора',xml});
  const previewPath=path.join(root,s.preview);
  const exists=await fs.access(previewPath).then(()=>true,()=>false);
  if(!only||only.test(s.id)||!exists)await target.screenshot({path:previewPath});
  results.push({id:s.id,xmlValid:xml.valid,...result});
}
// Обзор: сетка уменьшенных снимков, отрисованная браузером.
const cols=4,cw=360,ch=324;const rows=Math.ceil(manifest.length/cols);
const overview=`<!doctype html><html><head><meta charset="utf-8"><style>body{margin:0;background:#e9edf5}#grid{position:relative;width:1440px;height:${rows*ch}px}#grid img{position:absolute;max-width:336px;max-height:276px;box-shadow:0 2px 10px #14213a22;background:#fff}</style></head><body><div id="grid">${manifest.map((s,i)=>`<img src="${pathToFileURL(path.join(root,s.preview)).href}" style="left:${(i%cols)*cw+12}px;top:${Math.floor(i/cols)*ch+32}px" alt="">`).join('')}</div></body></html>`;
// Страница about:blank не может загружать file:// изображения, поэтому обзор открывается как временный файл.
const overviewFile=path.join(os.tmpdir(),`lidradar-overview-${process.pid}.html`);
await fs.writeFile(overviewFile,overview);
const overviewPage=await browser.newPage({viewport:{width:1440,height:rows*ch},deviceScaleFactor:1});
await overviewPage.goto(pathToFileURL(overviewFile).href);
await overviewPage.waitForFunction(()=>[...document.images].every(img=>img.complete&&img.naturalWidth>0));
await overviewPage.locator('#grid').screenshot({path:path.join(root,'preview','overview.png')});
await fs.rm(overviewFile,{force:true});
await browser.close();
await fs.writeFile(path.join(root,'qa-report.json'),JSON.stringify({checkedAt:new Date().toISOString(),screens:results,totalTextLayers:results.reduce((a,r)=>a+r.textLayers,0),errors:results.reduce((a,r)=>a+r.errors.length,0),notes:['Импорт в Figma намеренно не выполнялся: пользователь импортирует вручную.','PNG получены браузером с комплектным Inter.','Автоматическая проверка текста дополнена визуальным просмотром.']},null,2));
console.log(JSON.stringify(results.filter(r=>r.errors.length),null,2));
console.log(`Проверено ${results.length} макетов, ошибок: ${results.reduce((a,r)=>a+r.errors.length,0)}`);
if(results.some(r=>r.errors.length))process.exitCode=1;
