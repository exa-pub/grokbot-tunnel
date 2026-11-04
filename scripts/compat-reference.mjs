// Execute only named pure functions extracted from the local reference bundle.
// The desktop application and its bootstrap/network code are never evaluated;
// the streaming reader gets its file chunks from an in-memory stub.
import fs from 'node:fs';
import vm from 'node:vm';
import path from 'node:path';
import util from 'node:util';
import {isUtf8,isAscii} from 'node:buffer';
const base = process.argv[2] ?? 'ai.tmp/grok-bot/0.78.4/app/dist';
const daemon = fs.readFileSync(path.join(base,'local-exec-daemon/main.cjs'),'utf8');
function extract(name) {
 const forms=[`async function*${name}(`,`async function ${name}(`,`function ${name}(`,`var ${name}=class`];
 const form=forms.find(f=>daemon.includes(f));
 if(form===undefined) throw Error(`missing ${name}`);
 const start=daemon.indexOf(form);
 for(let end=daemon.indexOf('}',start);end>=0;end=daemon.indexOf('}',end+1)) {
  const code=daemon.slice(start,end+1);
  try { new vm.Script(form.startsWith('var ')?code:`(${code})`); return code; } catch {}
 }
 throw Error(`cannot parse ${name}`);
}
// Stand-ins for the node modules the streaming reader imports: the file is
// read from memory in 64 KiB chunks, as createReadStream would.
const chunked=(text,size)=>({async *[Symbol.asyncIterator](){const b=Buffer.from(text);for(let i=0;i<b.length;i+=size)yield b.subarray(i,i+size)}});
const context=vm.createContext({Buffer,Math,Aye:{TextDecoder:util.TextDecoder},OL:{isUtf8},wye:{createReadStream:(text,o)=>chunked(text,o.highWaterMark)}});
for(const name of ['fut','Rye','Cye','XSr','QSr','lfe','SN','hN','gut','Eye','gg','vut','Aut','Tye','pxt','mxt','Xb','dIe','Qb','zc','bye','hut','c_r','yye','f_r','p_r','Sut']) vm.runInContext(extract(name),context);
vm.runInContext('var aXt=9,cXt=9,xye=64*1024;',context);
// readTextStreaming as the Linux read executor runs it, file path = text.
context.stream=async(text,args)=>{const r=await context.Sut({signal:undefined},text,args,{kind:'strictUtf8',cacheEncoding:'utf8'},false,Buffer.byteLength(text));return {...r,content:context.Eye(r.content,r.truncated,args)}};
const cases=[{text:'',args:{}},{text:'one\ntwo\nthree\n',args:{offset:2,limit:1}},{text:'one\ntwo\n',args:{offset:20}},{text:'one\ntwo\nthree',args:{offset:-2}}];
const streamCases=[...cases,
 {text:'one\ntwo\nthree\n',args:{maxChars:5}},
 {text:'one\ntwo\nthree\n',args:{maxChars:7}},
 {text:'one\ntwo\nthree',args:{maxChars:100}},
 {text:'one\ntwo\nthree\nfour\n',args:{offset:2,maxChars:4}},
 {text:'one\ntwo\nthree\nfour\n',args:{offset:2,limit:1,maxChars:1}},
 {text:'one\ntwo\nthree\nfour',args:{offset:-2,maxChars:1}},
 {text:'one\ntwo\n',args:{offset:20,maxChars:1}},
 {text:'ab\r\ncd\r\nef\r\n',args:{maxChars:5}},
 {text:'ab\r\ncd\r\nef\r\n',args:{maxChars:4}},
 {text:'\u{1F600}\u{1F600}\nx',args:{maxChars:1}},
 {text:'a'.repeat(70000)+'\nb',args:{maxChars:10}},
 {text:'a'+'é'.repeat(40000),args:{maxChars:10}},
 {text:'﻿'+'a'.repeat(70000),args:{maxChars:10}},
 {text:'x\n'+'a'.repeat(140000)+'\ny',args:{offset:2,maxChars:70000}},
];
// How the reader decodes a file that is not valid UTF-8: lossy UTF-8 if sS
// finds it mostly UTF-8, Latin-1 otherwise (Cut).
vm.runInContext(extract('sS'),context);
vm.runInContext('var mHe=2;',context);
const fallback=['636166e920617520616974','d0bfd180d0b8d0b2d0b5d18220ff20d0bcd0b8d180','e28220616263','e697a5e69cace8aa9eff fe e697a5'.replace(/ /g,''),'61ff fe62'.replace(/ /g,'')].map(hex=>{const b=Buffer.from(hex,'hex'),lossy=new util.TextDecoder('utf-8').decode(b);return {hex,mostlyUtf8:context.sS(lossy),lossy}});
// Long texts are stored as runs of one character to keep the file small.
const runs=t=>[...t.matchAll(/(.)\1*/gsu)].map(m=>({char:m[1],count:[...m[0]].length}));
const streaming=[];
for(const c of streamCases){
 const {content,...r}=await context.stream(c.text,c.args);
 streaming.push(c.text.length>200?{textRuns:runs(c.text),args:c.args,result:{contentRuns:runs(content),...r}}:{text:c.text,args:c.args,result:{content,...r}});
}
// How getFormatForFile classifies the start of a file (THe/ZO): UTF-16 and
// UTF-32 by byte order mark or zero bytes, binary by NUL or control bytes,
// images by signature. jschardet (yHe) is not run: inputs that would reach it
// throw and are left out.
const fmt=vm.createContext({Buffer,Math,TextDecoder:util.TextDecoder,process:{platform:'linux'},JE:{isUtf8,isAscii},yHe:{default:{detect(){throw Error('jschardet')}}},
 Qp:{default:{decode:(b,e)=>new util.TextDecoder(e.startsWith('utf-16')?e:'utf-8').decode(b)}}});
vm.runInContext('var oS=8192,LKt=27,hHe="UTF-8",BKt=1024,UKt=/[^\\u0000-\\u007f\\ufffd]/u,dp="utf8",mHe=2,VO="\\uFFFD",Rl={CRLF:"CRLF",LF:"LF"},'+
 'NKt=new TextDecoder("utf-8",{fatal:false}),a5=t=>t,c5=t=>t.toLowerCase();',fmt);
for(const name of ['THe','XKt','ZO','eYt','rYt','sYt','YKt','ZKt','KKt','Xme','AHe','jKt','WO','sS','Yme']) vm.runInContext(extract(name),fmt);
const utf16=(s,be)=>{const b=Buffer.from(s,'utf16le');if(be)b.swap16();return b};
const utf32=(s,be)=>Buffer.concat([...s].map(c=>{const b=Buffer.alloc(4);be?b.writeUInt32BE(c.codePointAt(0)):b.writeUInt32LE(c.codePointAt(0));return b}));
const formatInputs=[Buffer.alloc(0),Buffer.from('hello\n'),Buffer.from('a'.repeat(9000)+'\0'),Buffer.from('\x1b[31mred\x1b[0m\n'.repeat(20)),
 Buffer.from('plain \x1b[1mbold\x1b[0m text that is mostly letters and spaces '.repeat(5)),Buffer.from('café \x1b[0m'),
 utf16('hello world\n'),utf16('hello world\n',true),Buffer.concat([Buffer.from([0xff,0xfe]),utf16('hi there')]),Buffer.concat([Buffer.from([0xfe,0xff]),utf16('hi there',true)]),
 Buffer.concat([Buffer.from([0xff,0xfe,0,0]),utf32('hi there')]),Buffer.concat([Buffer.from([0,0,0xfe,0xff]),utf32('hi',true)]),utf32('hello'),utf32('hello',true),
 Buffer.from('ab\0cd'),Buffer.from('ff\n','hex'),Buffer.from('89504e470d0a1a0a0000000d49484452','hex'),Buffer.from('ffd8ffe000104a464946','hex'),
 Buffer.from('474946383961010001000000','hex'),Buffer.from('52494646240000005745425056503820','hex'),Buffer.from('\x89PNG\r\n\x1a\nno zero bytes','latin1'),
 Buffer.from('\x01'.repeat(5)+'a'.repeat(95)),Buffer.from('\x01'.repeat(4)+'a'.repeat(96)),Buffer.from('ff'.repeat(40),'hex'),
 Buffer.concat([Buffer.from('ж'.repeat(4095)+'a'),Buffer.from([0xd0])]),Buffer.concat([Buffer.from('ж'.repeat(4095)+'a'),Buffer.from([0xd0,0xb6]),Buffer.from('\x01'.repeat(30))]),
 Buffer.from('d0bfd180d0b8d0b2d0b5d18220d0bcd0b8d180ff','hex'),Buffer.from('61ff6262','hex'),Buffer.from('a\0b\0c'),Buffer.from('\0a\0b\0c\0d')];
const format=[];
for(const b of formatInputs){try{const r=fmt.THe(b);format.push({hex:b.toString('hex'),encoding:r.encoding,isBinaryFile:r.isBinaryFile,isImageFile:r.isImageFile})}catch(e){if(e.message!=='jschardet')throw e}}
// The size prepareImageForModel scales an image to (zct).
vm.runInContext('var Gct=1024,Hs=8;'+extract('zct'),fmt);
const imageFit=[[1,1],[1024,1024],[1025,10],[4000,3000],[3840,2160],[2160,3840],[10000,9],[10000,7],[7,10000],[2000,8],[1281,800],[0,5],[1500,1501]].flatMap(([w,h])=>[1024,1280].map(side=>({width:w,height:h,side,result:fmt.zct(w,h,side)})));
const output={provenance:{version:'0.78.4',platform:'linux',method:'isolated pure functions from source; not desktop traffic',source:'local-exec-daemon/main.cjs',markers:['QSr','Rye','lfe','hN','Sut','Eye','ZO','zct']},ranges:cases.map(c=>({...c,result:context.QSr(c.text,c.text.split('\n').length,c.args)})),streaming,fallback,discoveryMissing:context.lfe('codex'),terminalHeader:context.hN({pid:42,cwd:'/workspace',command:'printf hello',status:'running',startedAt:'2026-01-01T00:00:00.000Z',runningForMs:0}),format,imageFit};
process.stdout.write(JSON.stringify(output,null,2)+'\n');
