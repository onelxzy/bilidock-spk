/*
 * BiliDock · DSM 桌面窗口
 */
Ext.define('SYNO.SDS.BiliDock.Instance', {
	extend: 'SYNO.SDS.AppInstance',
	appWindowName: 'SYNO.SDS.BiliDock.Main',
	constructor: function() {
		this.callParent(arguments);
	}
});

Ext.define('SYNO.SDS.BiliDock.Main', {
	extend: 'SYNO.SDS.AppWindow',
	appInstance: null,

	constructor: function(cfg) {
		this.appInstance = cfg.appInstance;

		var config = Ext.apply({
			resizable: true,
			maximizable: true,
			minimizable: true,
			width: 1120,
			height: 740,
			layout: 'fit',
			items: [
				new Ext.BoxComponent({
					height: "100%",
					html: '<iframe src="/webman/3rdparty/bilidock/index.cgi/" frameborder="0" marginheight="0" marginwidth="0" width="100%" height="100%"></iframe>'
				})
			]
		}, cfg);

		this.callParent([config]);
	}
});
