package attack

// script solves the challenge in the browser.
//
// SHA-256 is written out rather than taken from crypto.subtle, which only
// exists on a secure origin: the challenge has to work over plain http
// (how it is tested here, and how some sites are reached) and in old
// browsers, and what is served on the day of an attack cannot depend on
// either. It works in batches so the tab keeps painting its progress; a
// one-second loop looks like a hung site.
//
// If the page comes back within a minute of solving, the cookie is not
// being kept (private mode, cookies off): solving again would loop for
// ever with nothing saying why, so the page says why instead.
//
// No backquotes in here: this is a Go raw string.
const script = `(function(){
  var f=document.getElementById('mw-challenge');
  if(!f) return;
  var token=f.getAttribute('data-challenge'), bits=parseInt(f.getAttribute('data-bits'),10)||16;
  var pct=document.getElementById('mw-pct'), state=document.getElementById('mw-state');
  var K='mw-challenge-'+location.host;
  try{
    var before=parseInt(sessionStorage.getItem(K)||'0',10);
    if(before && Date.now()-before<60000){
      sessionStorage.removeItem(K);
      state.textContent=f.getAttribute('data-nocookie');
      return;
    }
  }catch(e){}
` + hashJS + `
  var n=0, expected=Math.pow(2,bits);
  function batch(){
    for(var end=n+2000; n<end; n++){
      if((firstWord(token+'|'+n)>>>(32-bits))===0){
        document.getElementById('mw-nonce').value=String(n);
        try{ sessionStorage.setItem(K,String(Date.now())); }catch(e){}
        state.textContent=f.getAttribute('data-done');
        f.submit();
        return;
      }
    }
    if(pct) pct.textContent=Math.min(99,Math.floor(n*100/expected))+'%';
    setTimeout(batch,0);
  }
  setTimeout(batch,0);
})();`

// hashJS is the arithmetic, kept apart so a test can run it with node and
// compare it with Go's. If the browser computed anything else it would
// never find a valid answer: the page would solve, come back, solve again,
// and the site would sit in a silent loop with its process healthy
// behind it. firstWord is the first 32 bits of SHA-256 over a string of
// one-byte characters, which is all a difficulty up to 24 bits reads.
const hashJS = `
  var KK=[0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
    0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
    0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
    0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
    0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
    0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
    0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
    0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2];
  function ror(x,n){ return (x>>>n)|(x<<(32-n)); }
  function firstWord(s){
    var m=[],i,j;
    for(i=0;i<s.length;i++) m.push(s.charCodeAt(i)&255);
    var bitLen=m.length*8;
    m.push(128);
    while(m.length%64!==56) m.push(0);
    m.push(0,0,0,0,(bitLen>>>24)&255,(bitLen>>>16)&255,(bitLen>>>8)&255,bitLen&255);
    var h0=0x6a09e667,h1=0xbb67ae85,h2=0x3c6ef372,h3=0xa54ff53a,h4=0x510e527f,h5=0x9b05688c,h6=0x1f83d9ab,h7=0x5be0cd19;
    var w=new Array(64);
    for(i=0;i<m.length;i+=64){
      for(j=0;j<16;j++) w[j]=(m[i+4*j]<<24)|(m[i+4*j+1]<<16)|(m[i+4*j+2]<<8)|m[i+4*j+3];
      for(j=16;j<64;j++){
        var x=w[j-15], y=w[j-2];
        w[j]=(w[j-16]+(ror(x,7)^ror(x,18)^(x>>>3))+w[j-7]+(ror(y,17)^ror(y,19)^(y>>>10)))|0;
      }
      var a=h0,b=h1,c=h2,d=h3,e=h4,f=h5,g=h6,h=h7;
      for(j=0;j<64;j++){
        var t1=(h+(ror(e,6)^ror(e,11)^ror(e,25))+((e&f)^(~e&g))+KK[j]+w[j])|0;
        var t2=((ror(a,2)^ror(a,13)^ror(a,22))+((a&b)^(a&c)^(b&c)))|0;
        h=g; g=f; f=e; e=(d+t1)|0; d=c; c=b; b=a; a=(t1+t2)|0;
      }
      h0=(h0+a)|0; h1=(h1+b)|0; h2=(h2+c)|0; h3=(h3+d)|0; h4=(h4+e)|0; h5=(h5+f)|0; h6=(h6+g)|0; h7=(h7+h)|0;
    }
    return h0>>>0;
  }
`
