-- gozer for Neovim 0.11+: the .gozer filetype, the gozer language server
-- (gopls plus templates) and a key to switch back to plain gopls.
local M = { current = nil }

function M.use(name)
  local other = name == 'gozer' and 'gopls' or 'gozer'
  vim.lsp.enable(other, false) -- stops its clients
  vim.lsp.enable(name) -- re-attaches already open buffers
  M.current = name
end

function M.toggle()
  M.use(M.current == 'gozer' and 'gopls' or 'gozer')
  vim.notify('Go language server: ' .. M.current)
end

-- opts.cmd: server command (default { 'gozer', 'lsp' }), e.g. add '-runtime', 'github.com/you/gozer'
-- opts.key: toggle mapping (default '<leader>lG'); opts.default: 'gozer' or 'gopls'
function M.setup(opts)
  opts = opts or {}
  vim.filetype.add({ extension = { gozer = 'gozer' } })
  -- markup: tree-sitter HTML; Go code and @-syntax: semantic tokens from the gozer server
  vim.api.nvim_create_autocmd('FileType', {
    pattern = 'gozer',
    callback = function(ev) pcall(vim.treesitter.start, ev.buf, 'html') end,
  })
  -- inherit the user's gopls settings: gozer passes them straight to gopls
  local cfg = vim.deepcopy(vim.lsp.config.gopls or {})
  cfg.cmd = opts.cmd or { 'gozer', 'lsp' }
  cfg.filetypes = { 'go', 'gomod', 'gowork', 'gotmpl', 'gozer' }
  vim.lsp.config('gozer', cfg)
  M.use(opts.default or 'gozer')
  vim.keymap.set('n', opts.key or '<leader>lG', M.toggle, { desc = 'Toggle gozer LSP / gopls' })
end

return M
