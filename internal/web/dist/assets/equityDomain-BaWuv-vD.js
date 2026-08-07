function o(e){const n=e.filter(Number.isFinite);if(n.length===0)return;const i=Math.min(...n),t=Math.max(...n),a=t===i?Math.abs(t)*.01||1:(t-i)*.08;return[i-a,t+a]}export{o as p};
