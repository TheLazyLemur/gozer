window.demo = {
  width: () => window.innerWidth,
  flash(el) {
    el.animate([{ background: '#ffd84d' }, { background: 'transparent' }], { duration: 800 });
  },
};
