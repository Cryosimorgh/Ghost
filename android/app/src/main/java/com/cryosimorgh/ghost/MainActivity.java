package com.cryosimorgh.ghost;

import android.Manifest;
import android.app.Activity;
import android.content.ActivityNotFoundException;
import android.content.ContentValues;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.graphics.Color;
import android.graphics.Typeface;
import android.media.AudioDeviceInfo;
import android.media.AudioManager;
import android.net.Uri;
import android.os.Build;
import android.os.Bundle;
import android.os.Environment;
import android.provider.MediaStore;
import android.view.Gravity;
import android.view.View;
import android.view.ViewGroup;
import android.webkit.DownloadListener;
import android.webkit.JavascriptInterface;
import android.webkit.PermissionRequest;
import android.webkit.ValueCallback;
import android.webkit.WebChromeClient;
import android.webkit.WebResourceRequest;
import android.webkit.WebView;
import android.webkit.WebViewClient;
import android.widget.Button;
import android.widget.EditText;
import android.widget.ImageView;
import android.widget.LinearLayout;
import android.widget.TextView;
import android.widget.Toast;

import org.json.JSONArray;
import org.json.JSONObject;

import java.io.OutputStream;
import java.util.HashMap;
import java.util.Map;
import java.util.UUID;
import android.util.Base64;

/** Android client for a shared HTTPS Ghost hub. No local server or TURN proxy. */
public final class MainActivity extends Activity {
    private static final int FILE_PICKER = 41;
    private static final int MIC_PERMISSION = 42;
    private WebView browser;
    private ValueCallback<Uri[]> filePicker;
    private PermissionRequest micRequest;
    private Uri hubOrigin;
    private LinearLayout home;
    private final Map<String, DownloadFile> transfers = new HashMap<>();
    private final int mint = Color.rgb(167,216,199);
    private final int quiet = Color.rgb(153,175,167);

    private static final class DownloadFile {
        Uri uri;
        OutputStream stream;
        DownloadFile(Uri uri, OutputStream stream) { this.uri=uri; this.stream=stream; }
    }
    @Override public void onCreate(Bundle state) {
        super.onCreate(state);
        getWindow().setStatusBarColor(Color.rgb(18,25,27));
        getWindow().setNavigationBarColor(Color.rgb(19,28,30));
        String saved = getPreferences(MODE_PRIVATE).getString("hub", "");
        if (validHub(saved)) openHub(saved); else showSetup(saved);
    }
    private int dp(float value) { return Math.round(getResources().getDisplayMetrics().density * value); }
    private TextView text(String value, int size, int color) {
        TextView t = new TextView(this); t.setText(value); t.setTextColor(color);
        t.setTextSize(size); t.setLineSpacing(dp(4), 1); return t;
    }
    private boolean validHub(String input) {
        try { Uri u = Uri.parse(input); return "https".equalsIgnoreCase(u.getScheme()) && u.getHost() != null && !u.getHost().isEmpty(); }
        catch (Exception e) { return false; }
    }
    private boolean allowed(String url) {
        try {
            Uri u=Uri.parse(url);
            return hubOrigin != null && "https".equalsIgnoreCase(u.getScheme()) &&
                hubOrigin.getHost().equalsIgnoreCase(u.getHost()) && hubOrigin.getPort()==u.getPort();
        } catch (Exception e) { return false; }
    }
    private void showSetup(String previous) {
        if (browser != null) { browser.destroy(); browser=null; }
        LinearLayout root = new LinearLayout(this); root.setOrientation(LinearLayout.VERTICAL);
        root.setBackgroundColor(Color.rgb(16,23,25)); root.setGravity(Gravity.CENTER);
        root.setPadding(dp(30),dp(25),dp(30),dp(25));
        ImageView icon = new ImageView(this); icon.setImageResource(R.drawable.ghost_icon);
        LinearLayout.LayoutParams img = new LinearLayout.LayoutParams(dp(90),dp(90)); img.bottomMargin=dp(20); root.addView(icon,img);
        TextView title=text("Ghost", 30, Color.rgb(235,244,239)); title.setTypeface(null, Typeface.BOLD); title.setGravity(Gravity.CENTER); root.addView(title);
        TextView subtitle=text("A private line, wherever you are.", 14, quiet); subtitle.setGravity(Gravity.CENTER);
        LinearLayout.LayoutParams sp=new LinearLayout.LayoutParams(-1,-2);sp.topMargin=dp(9);sp.bottomMargin=dp(36);root.addView(subtitle,sp);
        TextView label=text("YOUR SHARED HTTPS HUB", 11, mint); label.setLetterSpacing(.1f); root.addView(label);
        EditText url=new EditText(this);url.setSingleLine(true);url.setInputType(17);
        url.setHint("https://ghost.example.com");url.setHintTextColor(Color.rgb(102,130,122));
        url.setTextColor(Color.WHITE);url.setText(previous);url.setTextSize(15);
        LinearLayout.LayoutParams ep=new LinearLayout.LayoutParams(-1,dp(54));ep.topMargin=dp(8);root.addView(url,ep);
        Button connect=new Button(this);connect.setAllCaps(false);connect.setText("Open Ghost");
        connect.setTextColor(Color.rgb(17,41,33));connect.setBackgroundTintList(android.content.res.ColorStateList.valueOf(mint));
        LinearLayout.LayoutParams cp=new LinearLayout.LayoutParams(-1,dp(52));cp.topMargin=dp(22);root.addView(connect,cp);
        connect.setOnClickListener(v -> {
            String address=url.getText().toString().trim();
            if(!validHub(address)) { url.setError("Enter a valid HTTPS hub URL"); return; }
            getPreferences(MODE_PRIVATE).edit().putString("hub",address).apply();openHub(address);
        });
        TextView help=text("The desktop Go hub must be reachable over HTTPS. This app connects directly to the same hub as your friends; messages and media remain peer-to-peer.",12,quiet);
        help.setGravity(Gravity.CENTER);LinearLayout.LayoutParams hp=new LinearLayout.LayoutParams(-1,-2);hp.topMargin=dp(27);root.addView(help,hp);
        home=root;setContentView(root);
    }
    private void openHub(String address) {
        hubOrigin=Uri.parse(address);
        if(browser!=null){browser.destroy();browser=null;}
        browser=new WebView(this);browser.setBackgroundColor(Color.rgb(16,23,25));
        browser.getSettings().setJavaScriptEnabled(true);
        browser.getSettings().setDomStorageEnabled(true);
        browser.getSettings().setAllowFileAccess(false);
        browser.getSettings().setAllowContentAccess(true); // Android document picker
        browser.getSettings().setMediaPlaybackRequiresUserGesture(false);
        browser.getSettings().setJavaScriptCanOpenWindowsAutomatically(false);
        if(Build.VERSION.SDK_INT>=26) browser.getSettings().setSafeBrowsingEnabled(true);
        browser.addJavascriptInterface(new GhostBridge(),"GhostAndroid");
        browser.setWebViewClient(new WebViewClient() {
            @Override public boolean shouldOverrideUrlLoading(WebView view,WebResourceRequest request) {
                if(allowed(request.getUrl().toString())) return false;
                if(request.isForMainFrame()) {
                    try { startActivity(new Intent(Intent.ACTION_VIEW, request.getUrl())); } catch(Exception ignored) { }
                }
                return true;
            }
            @Override public void onReceivedError(WebView view, WebResourceRequest request, android.webkit.WebResourceError error) {
                if(request.isForMainFrame()) Toast.makeText(MainActivity.this,"Cannot reach hub. Check its HTTPS address.",Toast.LENGTH_LONG).show();
            }
        });
        browser.setWebChromeClient(new WebChromeClient() {
            @Override public void onPermissionRequest(PermissionRequest request) {
                runOnUiThread(() -> {
                    if(!allowed(request.getOrigin().toString())) { request.deny();return; }
                    boolean wantsMic=false;
                    for(String resource:request.getResources()) if(PermissionRequest.RESOURCE_AUDIO_CAPTURE.equals(resource)) wantsMic=true;
                    if(!wantsMic) {request.deny();return;}
                    micRequest=request;
                    if(checkSelfPermission(Manifest.permission.RECORD_AUDIO)==PackageManager.PERMISSION_GRANTED) grantMic();
                    else requestPermissions(new String[]{Manifest.permission.RECORD_AUDIO},MIC_PERMISSION);
                });
            }
            @Override public boolean onShowFileChooser(WebView view,ValueCallback<Uri[]> callback,FileChooserParams params) {
                if(filePicker!=null)filePicker.onReceiveValue(null);filePicker=callback;
                Intent intent=new Intent(Intent.ACTION_OPEN_DOCUMENT);intent.setType("*/*");
                intent.addCategory(Intent.CATEGORY_OPENABLE);
                try{startActivityForResult(intent,FILE_PICKER);}catch(ActivityNotFoundException e){filePicker.onReceiveValue(null);filePicker=null;return false;}
                return true;
            }
        });
        setContentView(browser);browser.loadUrl(address);
    }
    private void grantMic() {
        if(micRequest!=null){micRequest.grant(new String[]{PermissionRequest.RESOURCE_AUDIO_CAPTURE});micRequest=null;}
    }
    @Override public void onRequestPermissionsResult(int requestCode,String[] permissions,int[] grants){
        super.onRequestPermissionsResult(requestCode,permissions,grants);
        if(requestCode==MIC_PERMISSION){if(grants.length>0&&grants[0]==PackageManager.PERMISSION_GRANTED)grantMic();
            else if(micRequest!=null){micRequest.deny();micRequest=null;}}
    }
    @Override protected void onActivityResult(int requestCode,int resultCode,Intent data){
        super.onActivityResult(requestCode,resultCode,data);
        if(requestCode==FILE_PICKER&&filePicker!=null){filePicker.onReceiveValue(resultCode==RESULT_OK && data!=null && data.getData()!=null ? new Uri[]{data.getData()}:null);filePicker=null;}
    }
    @Override public void onBackPressed() {
        if(browser!=null && browser.canGoBack() && allowed(browser.getUrl())) browser.goBack();
        else if(browser!=null) {String url=getPreferences(MODE_PRIVATE).getString("hub","");showSetup(url);}
        else super.onBackPressed();
    }
    @Override protected void onDestroy() {
        if(micRequest!=null){micRequest.deny();micRequest=null;}
        if(filePicker!=null){filePicker.onReceiveValue(null);filePicker=null;}
        synchronized(transfers){for(DownloadFile f:transfers.values())try{f.stream.close();}catch(Exception ignored){}transfers.clear();}
        if(browser!=null){browser.destroy();browser=null;}
        super.onDestroy();
    }
    private final class GhostBridge {
        @JavascriptInterface public String listAudioRoutes() {
            JSONArray result=new JSONArray();
            try {
                if(Build.VERSION.SDK_INT>=31){
                    AudioManager mgr=(AudioManager)getSystemService(AUDIO_SERVICE);
                    for(AudioDeviceInfo d:mgr.getAvailableCommunicationDevices()){
                        JSONObject obj=new JSONObject();obj.put("id","native:"+d.getId());obj.put("label",String.valueOf(d.getProductName()));result.put(obj);
                    }
                }
            }catch(Exception ignored){}
            return result.toString();
        }
        @JavascriptInterface public boolean setAudioRoute(String route) {
            if(Build.VERSION.SDK_INT<31)return false;
            try {
                AudioManager mgr=(AudioManager)getSystemService(AUDIO_SERVICE);
                if(route==null||route.isEmpty()){mgr.clearCommunicationDevice();return true;}
                if(!route.startsWith("native:"))return false;
                int id=Integer.parseInt(route.substring(7));
                for(AudioDeviceInfo d:mgr.getAvailableCommunicationDevices()){
                    if(d.getId()==id){mgr.setMode(AudioManager.MODE_IN_COMMUNICATION);return mgr.setCommunicationDevice(d);}
                }
            }catch(Exception ignored){}
            return false;
        }
        @JavascriptInterface public String beginDownload(String filename,String mime) {
            try {
                String safe=filename.replaceAll("[\\\\/\\p{Cntrl}]","_");
                if(safe.length()>200)safe=safe.substring(0,200);
                if(safe.isEmpty())safe="Ghost-file";
                ContentValues values=new ContentValues();values.put(MediaStore.Downloads.DISPLAY_NAME,safe);
                values.put(MediaStore.Downloads.MIME_TYPE,mime==null||mime.isEmpty()?"application/octet-stream":mime);
                values.put(MediaStore.Downloads.RELATIVE_PATH,Environment.DIRECTORY_DOWNLOADS+"/Ghost");
                values.put(MediaStore.Downloads.IS_PENDING,1);
                Uri uri=getContentResolver().insert(MediaStore.Downloads.EXTERNAL_CONTENT_URI,values);
                if(uri==null)return "";
                OutputStream output=getContentResolver().openOutputStream(uri);
                if(output==null){getContentResolver().delete(uri,null,null);return "";}
                String key=UUID.randomUUID().toString();synchronized(transfers){transfers.put(key,new DownloadFile(uri,output));}
                return key;
            }catch(Exception e){return "";}
        }
        @JavascriptInterface public boolean appendDownload(String token,String base64) {
            try {synchronized(transfers){DownloadFile f=transfers.get(token);if(f==null)return false;
                f.stream.write(Base64.decode(base64,Base64.NO_WRAP));return true;}}
            catch(Exception e){abortDownload(token);return false;}
        }
        @JavascriptInterface public boolean completeDownload(String token) {
            DownloadFile f;synchronized(transfers){f=transfers.remove(token);}
            if(f==null)return false;
            try {
                f.stream.close();ContentValues v=new ContentValues();v.put(MediaStore.Downloads.IS_PENDING,0);
                getContentResolver().update(f.uri,v,null,null);return true;
            }catch(Exception e){getContentResolver().delete(f.uri,null,null);return false;}
        }
        @JavascriptInterface public void abortDownload(String token) {
            DownloadFile f;synchronized(transfers){f=transfers.remove(token);}
            if(f!=null){try{f.stream.close();}catch(Exception ignored){}getContentResolver().delete(f.uri,null,null);}
        }
    }
}
